package extloop

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/runtime"
)

func step() runtime.LoopStep {
	return runtime.LoopStep{
		Request: &model.Request{System: "指令", Messages: []model.Message{
			{Role: model.RoleUser, Content: []*v1.ContentBlock{
				{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "你好"}}},
				{Kind: &v1.ContentBlock_CardAction{CardAction: &v1.CardAction{CardId: "k", Action: "ok", Label: "好"}}},
				{Kind: &v1.ContentBlock_Card{Card: &v1.Card{}}},
			}},
		}, Tools: []model.ToolSpec{{Name: "note", InputSchema: []byte(`{"type":"object"}`)}}},
		SessionID: "s", RunID: "r", Attempt: 2, Turn: 3, State: []byte{0, 1, 2}, StateVersion: "v1",
		Previous: []runtime.PreviousCall{{Call: &v1.ToolCall{CallId: "call_x", Capability: "note"}, IsError: true}},
	}
}

func client(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	r := NewRegistry([]Server{{Name: "plan", BusinessLine: "bl", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer t"}, Timeout: DefaultTimeout}}, nil)
	l, err := r.Loop("bl", "plan")
	if err != nil {
		t.Fatal(err)
	}
	return l.(*Client)
}

func TestDecideJSON(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			t.Error("missing header")
		}
		b, _ := io.ReadAll(r.Body)
		var req v1.LoopStepRequest
		if err := protojson.Unmarshal(b, &req); err != nil {
			t.Error(err)
		}
		// 卡片不发送，卡片动作呈现为文本；"上一步"按位置给出改写后的 ID 与 is_error。
		blocks := req.GetMessages()[0].GetContent()
		if req.GetTurn() != 3 || string(req.GetState()) != "\x00\x01\x02" || len(blocks) != 2 || blocks[1].GetText() == nil ||
			req.GetPrevious().GetResults()[0].GetCallId() != "call_x" || !req.GetPrevious().GetResults()[0].GetIsError() ||
			req.GetTools()[0].GetInputSchemaJson() != `{"type":"object"}` {
			t.Errorf("request: %v", &req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"text":{"text":"好"}}],"toolCalls":[{"capability":"note","argumentsJson":"{}"}],` +
			`"state":"AQI=","stateVersion":"v2","reportedUsage":[{"model":"x/y","usage":{"inputTokens":"5"}}],"futureField":1}`))
	})
	got, err := c.Decide(context.Background(), step(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if model.Text(got.Content) != "好" || len(got.ToolCalls) != 1 || string(got.State) != "\x01\x02" || got.StateVersion != "v2" ||
		len(got.Reported) != 1 || got.Reported[0].Usage.GetInputTokens() != 5 {
		t.Fatalf("reply %+v", got)
	}
}

func TestDecideNDJSON(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"delta":{"text":"好"}}` + "\n" + `{"delta":{"reasoning":"想"}}` + "\n\n" +
			`{"response":{"content":[{"text":{"text":"好的"}}]}}` + "\n"))
	})
	var deltas []model.Delta
	got, err := c.Decide(context.Background(), step(), func(d model.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if model.Text(got.Content) != "好的" || len(deltas) != 2 || deltas[0].Text != "好" || deltas[1].Reasoning != "想" {
		t.Fatalf("reply %+v deltas %+v", got, deltas)
	}
}

// 流式缺少最后一行、最后一行之后还有内容、状态码不是 200、无法解析：这一步失败（ADR-0056 Q7、ADR-0062 第 4 条）。
// 错误信息不含回答内容。
func TestDecideFailures(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"no response line": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"delta":{"text":"秘密"}}` + "\n"))
		},
		"line after response": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"response":{}}` + "\n" + `{"delta":{"text":"秘密"}}` + "\n"))
		},
		"status": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "秘密", http.StatusBadGateway) },
		"unparsable": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"content":"秘密"}`))
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client(t, h).Decide(context.Background(), step(), nil)
			if err == nil || strings.Contains(err.Error(), "秘密") {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry([]Server{{Name: "plan", BusinessLine: "bl", URL: "http://x"}}, nil)
	if _, err := r.Loop("other", "plan"); !errors.Is(err, runtime.ErrLoopNotRegistered) {
		t.Fatalf("other business line: %v", err)
	}
	if _, err := r.Loop("bl", "plan"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOOP_TOKEN", "secret")
	write("a.yaml", "name: plan\nbusiness_line: bl\nurl: https://loop.example\nheaders: {Authorization: \"Bearer ${LOOP_TOKEN}\"}\n")
	write("b.yaml", "name: plan\nbusiness_line: other\nurl: https://loop.example\n")
	got, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Headers["Authorization"] != "Bearer secret" || got[0].Timeout != DefaultTimeout {
		t.Fatalf("servers %+v", got)
	}
	write("c.yaml", "name: plan\nbusiness_line: bl\nurl: https://dup.example\n")
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("duplicate (business_line, name) accepted")
	}
	if _, err := parse([]byte("name: plan\nbusiness_line: bl\nurl: https://x\nheaders: {A: \"${UNSET_LOOP_VAR}\"}\n"), os.Getenv); err == nil {
		t.Fatal("unset header variable accepted")
	}
	if _, err := parse([]byte("name: plan\nbusiness_line: bl\nurl: https://x\nmodel: m\n"), os.Getenv); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// 超过上限的流式回答报错，而不是截断后照常解析；请求失败的错误不含登记的地址（会进入 RunFailed 原因）。
func TestDecideLimitsAndErrors(t *testing.T) {
	big := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		line := []byte(`{"delta":{"text":"` + strings.Repeat("x", 1<<20) + `"}}` + "\n")
		for range 5 {
			_, _ = w.Write(line)
		}
		_, _ = w.Write([]byte(`{"response":{}}` + "\n"))
	})
	if _, err := big.Decide(context.Background(), step(), nil); !errors.Is(err, errTooLarge) {
		t.Fatalf("oversized stream: %v", err)
	}
	r := NewRegistry([]Server{{Name: "plan", BusinessLine: "bl", URL: "http://127.0.0.1:1/secret-path"}}, nil)
	l, _ := r.Loop("bl", "plan")
	if l.(*Client).Server.Timeout != DefaultTimeout {
		t.Fatal("registry left a zero timeout")
	}
	_, err := l.Decide(context.Background(), step(), nil)
	if err == nil || strings.Contains(err.Error(), "secret-path") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("request failure: %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("superseded")
	cancel(cause)
	if _, err := l.Decide(ctx, step(), nil); !errors.Is(err, cause) {
		t.Fatalf("cancel cause lost: %v", err)
	}
}
