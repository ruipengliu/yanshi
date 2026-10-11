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
