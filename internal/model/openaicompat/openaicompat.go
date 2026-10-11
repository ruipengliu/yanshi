// Package openaicompat 接入 OpenAI 兼容的 Chat Completions 接口。
// 火山方舟（Ark）与 vLLM 等私有化推理服务都提供该接口（ADR-0001）。
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

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
	// Client 为 nil 时使用 DefaultClient。
	Client *http.Client
	// StallTimeout 是流式响应两次收到数据之间的最长间隔（默认 2 分钟），超过时以 model.ErrStalled 结束：
	// 连接可能已经断开而未被察觉，不能让 Worker 无限等下去。
	StallTimeout time.Duration
}

// DefaultClient 是调用模型的 HTTP 客户端。http.DefaultClient 没有建连与等待响应头的时限，且每个主机只保留
// 2 个空闲连接：并发的流式调用多了，每次都要重新握手。整次调用的时限由 model.Guard 控制，这里不设。
var DefaultClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 2 * time.Minute,
	MaxIdleConns:          1024,
	MaxIdleConnsPerHost:   256,
	IdleConnTimeout:       90 * time.Second,
	ForceAttemptHTTP2:     true,
}}

func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return DefaultClient
}

// stallReader 每收到数据就推迟定时器；定时器触发时请求被取消，阻塞的读取随之返回。
type stallReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (s *stallReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 {
		s.t.Reset(s.d)
	}
	return n, err
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wirePart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ImageURL   *wireImageURL   `json:"image_url,omitempty"`
	InputAudio *wireInputAudio `json:"input_audio,omitempty"`
}

type wireInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
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
	MaxTokens   int  `json:"max_tokens,omitempty"`
	Logprobs    bool `json:"logprobs,omitempty"`
	TopLogprobs int  `json:"top_logprobs,omitempty"`
	// ReasoningEffort 取自模型 ID 的后缀（SplitEffort）；为空时不发送，由模型按默认方式思考。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// efforts 是可以写在模型 ID 后缀上的思考强度（OpenAI 与火山方舟的 reasoning_effort 取值）。
// 只认这几个值：其他带冒号的模型 ID 原样发送。
var efforts = map[string]bool{"minimal": true, "low": true, "medium": true, "high": true}

// SplitEffort 拆开 "doubao-seed-2-1-lite-260915:minimal" 形式的模型 ID。思考强度是 AgentDef 的选择
// （docs/design/model-calls.md §4），按评测结果选取：强度越低首字越快，但过低时模型不按指令使用工具。
func SplitEffort(modelID string) (id, effort string) {
	if i := strings.LastIndexByte(modelID, ':'); i > 0 && efforts[modelID[i+1:]] {
		return modelID[:i], modelID[i+1:]
	}
	return modelID, ""
}

type wireChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		Logprobs *struct {
			Content []struct {
				TopLogprobs []struct {
					Token   string  `json:"token"`
					Logprob float64 `json:"logprob"`
				} `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
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

// audioFormats 是可以以 input_audio 发送的音频类型（火山方舟实测支持 wav、mp3、m4a，不支持 webm 与 ogg）。
var audioFormats = map[string]string{"audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/mpeg": "mp3", "audio/mp3": "mp3", "audio/mp4": "m4a", "audio/x-m4a": "m4a", "audio/m4a": "m4a"}

// inputAudio 返回可直接发给模型的音频；只有 Worker 内联了数据的音频可以发送。
func inputAudio(m *v1.Media) *wireInputAudio {
	f := audioFormats[m.GetMimeType()]
	if f == "" || len(m.GetData()) == 0 {
		return nil
	}
	return &wireInputAudio{Data: base64.StdEncoding.EncodeToString(m.GetData()), Format: f}
}

// content 编码消息内容。只有 allowMedia（用户消息）时图片与音频才以 image_url、input_audio 发送；
// 其余非文本内容（含工具结果中的图片，多数模型接口不支持）呈现为一行描述文本。
func content(blocks []*v1.ContentBlock, allowMedia bool) any {
	var parts []wirePart
	hasMedia := false
	for _, b := range blocks {
		switch k := b.GetKind().(type) {
		case *v1.ContentBlock_Text:
			parts = append(parts, wirePart{Type: "text", Text: k.Text.GetText()})
		case *v1.ContentBlock_UiContext:
			parts = append(parts, wirePart{Type: "text", Text: model.UIContextText(k.UiContext)})
		case *v1.ContentBlock_CardAction:
			parts = append(parts, wirePart{Type: "text", Text: model.CardActionText(k.CardAction)})
		// Card 不发给模型：卡片是模型自己经 show_card 写的，结果中另有一行说明（card.ResultText）。
		case *v1.ContentBlock_Media:
			if url := imageURL(k.Media); allowMedia && url != "" {
				parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireImageURL{URL: url}})
				hasMedia = true
			} else if a := inputAudio(k.Media); allowMedia && a != nil {
				// 音频之外再给一行描述：模型据此知道这是一条语音消息、附件名是什么。
				parts = append(parts, wirePart{Type: "text", Text: model.DescribeMedia(k.Media)}, wirePart{Type: "input_audio", InputAudio: a})
				hasMedia = true
			} else {
				parts = append(parts, wirePart{Type: "text", Text: model.DescribeMedia(k.Media)})
			}
		}
	}
	if hasMedia {
		return parts
	}
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.Text
	}
	return strings.Join(texts, "\n")
}

func encode(req *model.Request) *wireRequest {
	id, effort := SplitEffort(req.Model)
	w := &wireRequest{Model: id, Stream: true, MaxTokens: req.MaxTokens, ReasoningEffort: effort}
	w.StreamOptions.IncludeUsage = true
	if req.TopLogprobs > 0 {
		w.Logprobs, w.TopLogprobs = true, req.TopLogprobs
	}
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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if p.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := p.client().Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 错误正文只用来识别错误码与"上下文超长"，不放进错误：它可能回显请求内容（model.ProviderError）。
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var err error = &model.ProviderError{Model: req.Model, Status: resp.StatusCode, Code: model.ErrorCode(b),
			RequestID: requestID(resp), RetryAfter: model.RetryAfter(resp.Header, time.Now())}
		if resp.StatusCode == http.StatusBadRequest && contextOverflow(string(b)) {
			err = fmt.Errorf("%w: %w", model.ErrContextOverflow, err)
		}
		return nil, err
	}
	stall := p.StallTimeout
	if stall <= 0 {
		stall = 2 * time.Minute
	}
	t := time.AfterFunc(stall, func() { cancel(model.ErrStalled) })
	defer t.Stop()
	out, err := decodeStream(&stallReader{r: resp.Body, t: t, d: stall}, req.Model, onDelta)
	if err != nil && errors.Is(context.Cause(ctx), model.ErrStalled) {
		return nil, fmt.Errorf("model %s: %w (no data for %s)", req.Model, model.ErrStalled, stall)
	}
	return out, err
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
			var err error = &model.ProviderError{Model: modelID, Code: model.ErrorCode([]byte(data))}
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
			if lp := ch.Logprobs; lp != nil {
				for _, pos := range lp.Content {
					top := map[string]float64{}
					for _, t := range pos.TopLogprobs {
						top[t.Token] = t.Logprob
					}
					out.Logprobs = append(out.Logprobs, top)
				}
			}
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

// requestID 返回提供商在响应头中给出的请求 ID（排查时向提供商查询）。
func requestID(resp *http.Response) string {
	for _, h := range []string{"X-Request-Id", "Request-Id", "X-Tc-Requestid"} {
		if v := resp.Header.Get(h); v != "" {
			return model.SafeCode(v)
		}
	}
	return ""
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

// Embed 调用 OpenAI 兼容的 /embeddings 接口。火山方舟的多模态嵌入模型（doubao-embedding-vision-*）
// 只提供 /embeddings/multimodal：一次请求的全部输入合成一个向量，所以每段文本单独请求（multimodalEmbed）。
func (p *Provider) Embed(ctx context.Context, modelID string, texts []string) ([][]float32, error) {
	if strings.Contains(modelID, "embedding-vision") {
		return p.multimodalEmbed(ctx, modelID, texts)
	}
	body, err := json.Marshal(map[string]any{"model": modelID, "input": texts, "encoding_format": "float"})
	if err != nil {
		return nil, err
	}
	resp, err := p.postEmbed(ctx, "/embeddings", modelID, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed %s: %w", modelID, err)
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embed %s: index %d out of range", modelID, d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if len(v) == 0 {
			return nil, fmt.Errorf("embed %s: no embedding for input %d", modelID, i)
		}
	}
	return vecs, nil
}

// multimodalEmbedConcurrency 是多模态嵌入的并发请求数：每段文本一次请求，Memory 召回一次只嵌入少量文本。
const multimodalEmbedConcurrency = 4

func (p *Provider) multimodalEmbed(ctx context.Context, modelID string, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	errs := make([]error, len(texts))
	sem := make(chan struct{}, multimodalEmbedConcurrency)
	var wg sync.WaitGroup
	for i, text := range texts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			vecs[i], errs[i] = p.multimodalEmbedOne(ctx, modelID, text)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return vecs, nil
}

func (p *Provider) multimodalEmbedOne(ctx context.Context, modelID, text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{"model": modelID, "input": []map[string]string{{"type": "text", "text": text}}})
	if err != nil {
		return nil, err
	}
	resp, err := p.postEmbed(ctx, "/embeddings/multimodal", modelID, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed %s: %w", modelID, err)
	}
	if len(out.Data.Embedding) == 0 {
		return nil, fmt.Errorf("embed %s: no embedding", modelID)
	}
	return out.Data.Embedding, nil
}

// postEmbed 发出嵌入请求；非 200 时返回 ProviderError（不含正文：它可能回显输入）。
func (p *Provider) postEmbed(ctx context.Context, path, modelID string, body []byte) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := p.client().Do(hreq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embed: %w", &model.ProviderError{Model: modelID, Status: resp.StatusCode, Code: model.ErrorCode(b),
			RequestID: requestID(resp), RetryAfter: model.RetryAfter(resp.Header, time.Now())})
	}
	return resp, nil
}
