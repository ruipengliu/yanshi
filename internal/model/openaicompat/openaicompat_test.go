package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

func TestGenerateStreamsTextAndToolCalls(t *testing.T) {
	var got wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		chunks := []string{
			`{"model":"m-1","choices":[{"delta":{"content":"你"}}]}`,
			`{"choices":[{"delta":{"content":"好"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"clock_now","arguments":"{\"tz\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":5}}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := &Provider{BaseURL: srv.URL, APIKey: "k"}
	var deltas []string
	resp, err := p.Generate(context.Background(), &model.Request{
		Model:  "m",
		System: "sys",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: model.TextBlocks("hi")},
			{Role: model.RoleAssistant, ToolCalls: []*v1.ToolCall{{CallId: "c0", Capability: "x", ArgumentsJson: "{}"}}},
			{Role: model.RoleTool, ToolCallID: "c0", Content: model.TextBlocks("ok")},
		},
		Tools: []model.ToolSpec{{Name: "clock_now", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}, func(d model.Delta) { deltas = append(deltas, d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "你好" || model.Text(resp.Content) != "你好" {
		t.Fatalf("deltas %v content %q", deltas, model.Text(resp.Content))
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].GetCallId() != "c1" || resp.ToolCalls[0].GetArgumentsJson() != `{"tz":"UTC"}` {
		t.Fatalf("tool calls %v", resp.ToolCalls)
	}
	if resp.Usage.GetInputTokens() != 7 || resp.Usage.GetCachedInputTokens() != 5 || resp.Model != "m-1" {
		t.Fatalf("usage %v model %s", resp.Usage, resp.Model)
	}
	if len(got.Messages) != 4 || got.Messages[0].Role != "system" || got.Messages[2].ToolCalls[0].ID != "c0" || got.Messages[3].ToolCallID != "c0" {
		t.Fatalf("encoded messages %+v", got.Messages)
	}
	if len(got.Tools) != 1 || !got.Stream {
		t.Fatalf("encoded request %+v", got)
	}
}

func TestGenerateHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := (&Provider{BaseURL: srv.URL}).Generate(context.Background(), &model.Request{Model: "m"}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

// 提供商的错误消息可能回显请求内容：错误只带状态码、错误码与请求 ID，进入日志与 RunFailed 的文本里没有消息原文。
func TestProviderErrorsOmitMessages(t *testing.T) {
	const secret = "我的身份证号 110101199001011234"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" && r.Header.Get("X-Stream") != "" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"error":{"code":"content_filter","message":"blocked: ` + secret + `"}}` + "\n\n"))
			return
		}
		w.Header().Set("X-Request-Id", "req-42")
		http.Error(w, `{"error":{"code":"invalid_request","type":"invalid_request_error","message":"bad input: `+secret+`"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	p := &Provider{BaseURL: srv.URL}
	_, err := p.Generate(context.Background(), &model.Request{Model: "m"}, nil)
	var pe *model.ProviderError
	if !errors.As(err, &pe) || pe.Status != 400 || pe.Code != "invalid_request/invalid_request_error" || pe.RequestID != "req-42" {
		t.Fatalf("err = %v", err)
	}
	_, embedErr := p.Embed(context.Background(), "e", []string{"x"})
	p.Client = &http.Client{Transport: headerTransport{"X-Stream", "1"}}
	_, streamErr := p.Generate(context.Background(), &model.Request{Model: "m"}, nil)
	if !errors.As(streamErr, &pe) || pe.Code != "content_filter" {
		t.Fatalf("stream err = %v", streamErr)
	}
	for _, e := range []error{err, embedErr, streamErr} {
		if e == nil || strings.Contains(e.Error(), "身份证") || strings.Contains(e.Error(), "bad input") {
			t.Errorf("error text leaks the provider message: %v", e)
		}
	}
}

type headerTransport struct{ k, v string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.k, h.v)
	return http.DefaultTransport.RoundTrip(r)
}

func TestGenerateContextOverflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := (&Provider{BaseURL: srv.URL}).Generate(context.Background(), &model.Request{Model: "m"}, nil)
	if !errors.Is(err, model.ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
}

func TestMediaEncoding(t *testing.T) {
	img := &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{
		MimeType: "image/png", Name: "chart.png", Size: 2048, Uri: "artifact://art_1", Data: []byte{0x89, 'P'},
	}}}
	w := encode(&model.Request{Model: "m", Messages: []model.Message{
		{Role: model.RoleUser, Content: append(model.TextBlocks("看图"), img)},
		{Role: model.RoleAssistant, ToolCalls: []*v1.ToolCall{{CallId: "c", Capability: "x", ArgumentsJson: "{}"}}},
		{Role: model.RoleTool, ToolCallID: "c", Content: append(model.TextBlocks("exported"), img)},
	}})
	parts, ok := w.Messages[0].Content.([]wirePart)
	if !ok || len(parts) != 2 || parts[1].Type != "image_url" || !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("user content = %#v", w.Messages[0].Content)
	}
	tool, ok := w.Messages[2].Content.(string)
	if !ok || !strings.Contains(tool, "[artifact art_1: chart.png, image/png, 2.0 KB]") {
		t.Fatalf("tool content = %#v", w.Messages[2].Content)
	}
}

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/embeddings" || req.Model != "emb" || len(req.Input) != 2 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// 故意乱序返回，验证按 index 归位。
		fmt.Fprint(w, `{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`)
	}))
	defer srv.Close()
	gw := model.NewGateway()
	gw.Register("p", &Provider{BaseURL: srv.URL})
	vecs, err := gw.Embed(context.Background(), "p/emb", []string{"a", "b"})
	if err != nil || len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Fatalf("vecs = %v, %v", vecs, err)
	}
}

// TestMultimodalEmbed：火山方舟的多模态嵌入模型每段文本单独请求 /embeddings/multimodal。
func TestMultimodalEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
			Input []struct {
				Type, Text string
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/embeddings/multimodal" || req.Model != "doubao-embedding-vision-251215" || len(req.Input) != 1 || req.Input[0].Type != "text" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		v := map[string]string{"a": "[1,0]", "b": "[0,1]"}[req.Input[0].Text]
		fmt.Fprintf(w, `{"data":{"embedding":%s}}`, v)
	}))
	defer srv.Close()
	gw := model.NewGateway()
	gw.Register("ark", &Provider{BaseURL: srv.URL})
	vecs, err := gw.Embed(context.Background(), "ark/doubao-embedding-vision-251215", []string{"a", "b"})
	if err != nil || len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Fatalf("vecs = %v, %v", vecs, err)
	}
}

// TestReasoningEffortSuffix：模型 ID 后缀是思考强度，以 reasoning_effort 发送；不认识的后缀原样保留。
func TestReasoningEffortSuffix(t *testing.T) {
	if w := encode(&model.Request{Model: "doubao-seed-2-1-lite-260915:minimal"}); w.Model != "doubao-seed-2-1-lite-260915" || w.ReasoningEffort != "minimal" {
		t.Fatalf("model %q effort %q", w.Model, w.ReasoningEffort)
	}
	if w := encode(&model.Request{Model: "qwen:7b"}); w.Model != "qwen:7b" || w.ReasoningEffort != "" {
		t.Fatalf("model %q effort %q", w.Model, w.ReasoningEffort)
	}
	b, _ := json.Marshal(encode(&model.Request{Model: "m"}))
	if strings.Contains(string(b), "reasoning_effort") {
		t.Fatalf("effort sent without suffix: %s", b)
	}
}

// TestAudioEncoding：用户消息中内联了数据的音频以 input_audio 发送，并附一行描述；没有数据或格式不支持时只有描述。
func TestAudioEncoding(t *testing.T) {
	wav := &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: "audio/wav", Name: "voice.wav", Size: 4, Uri: "artifact://art_2", Data: []byte("RIFF")}}}
	webm := &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: "audio/webm", Name: "v.webm", Size: 4, Uri: "artifact://art_3", Data: []byte("1234")}}}
	w := encode(&model.Request{Model: "m", Messages: []model.Message{{Role: model.RoleUser, Content: []*v1.ContentBlock{wav, webm}}}})
	parts, ok := w.Messages[0].Content.([]wirePart)
	if !ok || len(parts) != 3 || parts[1].Type != "input_audio" || parts[1].InputAudio.Format != "wav" || parts[1].InputAudio.Data != "UklGRg==" || parts[2].Type != "text" {
		t.Fatalf("content = %#v", w.Messages[0].Content)
	}
}

// TestUIContextEncoding：界面上下文以带说明的文本发给模型，位于用户的话之前；估算与之一致。
func TestUIContextEncoding(t *testing.T) {
	ui := &v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{Screen: "订单详情", Ref: "order://A1029", Content: "预计 10 月 14 日送达"}}}
	blocks := append([]*v1.ContentBlock{ui}, model.TextBlocks("这个什么时候到？")...)
	w := encode(&model.Request{Model: "m", Messages: []model.Message{{Role: model.RoleUser, Content: blocks}}})
	text, ok := w.Messages[0].Content.(string)
	if !ok || !strings.HasPrefix(text, "[界面上下文") || !strings.Contains(text, "界面：订单详情\n对象：order://A1029\n内容：预计 10 月 14 日送达\n[界面上下文结束]") ||
		!strings.HasSuffix(text, "这个什么时候到？") || strings.Contains(text, "选中：") {
		t.Fatalf("user content = %q", text)
	}
	if got, want := model.EstimateTokens(blocks), model.EstimateText(model.UIContextText(ui.GetUiContext()))+model.EstimateText("这个什么时候到？"); got != want {
		t.Fatalf("estimate %d, want %d", got, want)
	}
}

// TestStalledStreamFails：流式响应在 StallTimeout 内没有任何数据时以 ErrStalled 结束，而不是无限等待。
func TestStalledStreamFails(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select { // 之后不再发送任何数据
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p := &Provider{BaseURL: srv.URL, StallTimeout: 50 * time.Millisecond}
	start := time.Now()
	_, err := p.Generate(context.Background(), &model.Request{Model: "m"}, nil)
	if !errors.Is(err, model.ErrStalled) || !model.Retryable(err) {
		t.Fatalf("err = %v, want a retryable ErrStalled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stall detected after %s", d)
	}
}

func TestRetryAfterIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	}))
	defer srv.Close()
	_, err := (&Provider{BaseURL: srv.URL}).Generate(context.Background(), &model.Request{Model: "m"}, nil)
	var pe *model.ProviderError
	if !errors.As(err, &pe) || pe.Status != 429 || pe.RetryAfter != 7*time.Second || !model.Retryable(err) {
		t.Fatalf("err = %#v", err)
	}
}

// 请求 TopLogprobs 时发送 logprobs 参数，并读回各输出位置上的候选 token 概率（判断用的小模型，m6-decisions §4）。
func TestLogprobs(t *testing.T) {
	var got wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"A"},"logprobs":{"content":[{"token":"A","logprob":-0.1,"top_logprobs":[{"token":"A","logprob":-0.1},{"token":"B","logprob":-2.4}]}]}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := &Provider{BaseURL: srv.URL}
	resp, err := p.Generate(context.Background(), &model.Request{Model: "m", Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks("q")}},
		MaxTokens: 1, TopLogprobs: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Logprobs || got.TopLogprobs != 5 || got.MaxTokens != 1 {
		t.Fatalf("request %+v", got)
	}
	if len(resp.Logprobs) != 1 || resp.Logprobs[0]["A"] != -0.1 || resp.Logprobs[0]["B"] != -2.4 {
		t.Fatalf("logprobs %v", resp.Logprobs)
	}
}

// TestReasoningStream：思考过程（reasoning_content）以单独的增量交出并汇总到 Response.Reasoning；结束原因与思考 token
// 一并记录（ADR-0034）。思考过程不进入之后的请求：encode 只发送 Content。
func TestReasoningStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range []string{
			`{"choices":[{"delta":{"reasoning_content":"用户问"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"时间"}}]}`,
			`{"choices":[{"delta":{"content":"现在"}}]}`,
			`{"choices":[{"delta":{"content":"七点"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":6,"completion_tokens_details":{"reasoning_tokens":4}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	var text, reasoning string
	resp, err := (&Provider{BaseURL: srv.URL}).Generate(context.Background(), &model.Request{Model: "m"}, func(d model.Delta) {
		text += d.Text
		reasoning += d.Reasoning
	})
	if err != nil || text != "现在七点" || reasoning != "用户问时间" || resp.Reasoning != "用户问时间" || model.Text(resp.Content) != "现在七点" {
		t.Fatalf("text %q reasoning %q resp %+v err %v", text, reasoning, resp, err)
	}
	if resp.FinishReason != "stop" || resp.Usage.GetReasoningTokens() != 4 {
		t.Fatalf("finish %q usage %v", resp.FinishReason, resp.Usage)
	}
}
