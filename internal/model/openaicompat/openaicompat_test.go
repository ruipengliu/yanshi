package openaicompat

import (
	"context"
	"encoding/json"
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
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
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
	if resp.Usage.GetInputTokens() != 7 || resp.Model != "m-1" {
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
