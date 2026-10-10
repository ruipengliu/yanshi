// Package openaicompat 接入 OpenAI 兼容的 Chat Completions 接口。
// 火山方舟（Ark）与 vLLM 等私有化推理服务都提供该接口（ADR-0001）。
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

// ArkBaseURL 是火山方舟（北京区域）的默认地址。
const ArkBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// TokenHubBaseURL 是腾讯云 TokenHub 的 OpenAI 兼容地址。
const TokenHubBaseURL = "https://tokenhub.tencentmaas.com/v1"

type Provider struct {
	// BaseURL 不含 /chat/completions 后缀。
	BaseURL string
	APIKey  string
	Client  *http.Client
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wirePart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type wireRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Tools         []wireTool    `json:"tools,omitempty"`
	Stream        bool          `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type wireChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        uint64 `json:"prompt_tokens"`
		CompletionTokens    uint64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens uint64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// imageURL 返回可直接发给模型的图片地址；不可直接发送时返回空串。
func imageURL(m *v1.Media) string {
	if !strings.HasPrefix(m.GetMimeType(), "image/") {
		return ""
	}
	if len(m.GetData()) > 0 {
		return "data:" + m.GetMimeType() + ";base64," + base64.StdEncoding.EncodeToString(m.GetData())
	}
	if u := m.GetUri(); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}

// content 编码消息内容。只有 allowImages（用户消息）时图片才以 image_url 发送；
// 其余非文本内容（含工具结果中的图片，多数模型接口不支持）呈现为一行描述文本。
func content(blocks []*v1.ContentBlock, allowImages bool) any {
	var parts []wirePart
	hasImage := false
	for _, b := range blocks {
		switch k := b.GetKind().(type) {
		case *v1.ContentBlock_Text:
			parts = append(parts, wirePart{Type: "text", Text: k.Text.GetText()})
		case *v1.ContentBlock_UiContext:
			parts = append(parts, wirePart{Type: "text", Text: model.UIContextText(k.UiContext)})
		case *v1.ContentBlock_Media:
			if url := imageURL(k.Media); allowImages && url != "" {
				parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireImageURL{URL: url}})
				hasImage = true
			} else {
				parts = append(parts, wirePart{Type: "text", Text: model.DescribeMedia(k.Media)})
			}
		}
	}
	if hasImage {
		return parts
	}
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.Text
	}
	return strings.Join(texts, "\n")
}

func encode(req *model.Request) *wireRequest {
	w := &wireRequest{Model: req.Model, Stream: true}
	w.StreamOptions.IncludeUsage = true
	if req.System != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		if len(m.Content) > 0 || m.Role != model.RoleAssistant {
			wm.Content = content(m.Content, m.Role == model.RoleUser)
		}
		for _, tc := range m.ToolCalls {
			var c wireToolCall
			c.ID, c.Type = tc.GetCallId(), "function"
			c.Function.Name, c.Function.Arguments = tc.GetCapability(), tc.GetArgumentsJson()
			wm.ToolCalls = append(wm.ToolCalls, c)
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name, wt.Function.Description, wt.Function.Parameters = t.Name, t.Description, t.InputSchema
		w.Tools = append(w.Tools, wt)
	}
	return w
}

func (p *Provider) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	body, err := json.Marshal(encode(req))
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if p.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("model %s: http %d: %s", req.Model, resp.StatusCode, bytes.TrimSpace(b))
		if resp.StatusCode == http.StatusBadRequest && contextOverflow(string(b)) {
			err = fmt.Errorf("%w: %w", model.ErrContextOverflow, err)
		}
		return nil, err
	}
	return decodeStream(resp.Body, req.Model, onDelta)
}

func decodeStream(r io.Reader, modelID string, onDelta func(model.Delta)) (*model.Response, error) {
	out := &model.Response{Model: modelID, Usage: &v1.Usage{}}
	var text strings.Builder
	calls := map[int]*v1.ToolCall{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var c wireChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return nil, fmt.Errorf("model %s: bad stream chunk: %w", modelID, err)
		}
		if c.Error != nil {
			err := fmt.Errorf("model %s: %s", modelID, c.Error.Message)
			if contextOverflow(c.Error.Message) {
				err = fmt.Errorf("%w: %w", model.ErrContextOverflow, err)
			}
			return nil, err
		}
		if c.Model != "" {
			out.Model = c.Model
		}
		if c.Usage != nil {
			out.Usage = &v1.Usage{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens}
			if d := c.Usage.PromptTokensDetails; d != nil {
				out.Usage.CachedInputTokens = d.CachedTokens
			}
		}
		for _, ch := range c.Choices {
			if d := ch.Delta.Content; d != "" {
				text.WriteString(d)
				if onDelta != nil {
					onDelta(model.Delta{Text: d})
				}
			}
			for i, tc := range ch.Delta.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				call := calls[idx]
				if call == nil {
					call = &v1.ToolCall{}
					calls[idx] = call
				}
				if tc.ID != "" {
					call.CallId = tc.ID
				}
				if tc.Function.Name != "" {
					call.Capability = tc.Function.Name
				}
				call.ArgumentsJson += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("model %s: read stream: %w", modelID, err)
	}
	if text.Len() > 0 {
		out.Content = model.TextBlocks(text.String())
	}
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		c := calls[i]
		if c.ArgumentsJson == "" {
			c.ArgumentsJson = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, c)
	}
	return out, nil
}

// contextOverflow 识别"上下文超长"错误。各供应商没有统一的错误码，只能按文本匹配：
// OpenAI 与 vLLM 使用 context_length_exceeded / "maximum context length"。
// 火山方舟的具体形式尚未用真实请求确认（docs/design/m2-long-runs.md §9）。
func contextOverflow(body string) bool {
	b := strings.ToLower(body)
	for _, s := range []string{"context_length_exceeded", "maximum context length", "context length", "context window", "too many tokens", "prompt is too long"} {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

// Embed 调用 OpenAI 兼容的 /embeddings 接口。火山方舟多模态嵌入模型的接口形式待用真实请求确认
// （docs/design/m4-memory-grant.md §10）。
func (p *Provider) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": texts, "encoding_format": "float"})
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embed %s: http %d: %s", model, resp.StatusCode, bytes.TrimSpace(b))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed %s: %w", model, err)
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embed %s: index %d out of range", model, d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if len(v) == 0 {
			return nil, fmt.Errorf("embed %s: no embedding for input %d", model, i)
		}
	}
	return vecs, nil
}
