package looptest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/extloop"
	"yanshi/internal/model"
	"yanshi/internal/model/openaicompat"
)

// refLoopOptions 控制 Go 参考外部循环的回答模式以及用于验证套件的故障注入（坏变体）。
type refLoopOptions struct {
	modelURL string
	stream   bool
	// earlyHeaders 在调用模型之前就发出 200 与 NDJSON 响应头（常见于先开流的框架）：模型出错时只能以缺少最后一行表示失败。
	earlyHeaders       bool
	onlyFirstCall      bool
	cacheByRunTurn     bool
	dropSystem         bool
	reverseToolResults bool
	dropAfterTool      bool
	okOnModelFailure   bool
	messagesInState    bool
	sessionScopedState bool
	dropStepToken      bool
}

type refLoopState struct {
	RunID    string            `json:"run_id"`
	Steps    int               `json:"steps"`
	Messages []*v1.LoopMessage `json:"messages,omitempty"`
}

// refLoop 是最简 Go 外部循环服务：把 LoopStepRequest 转成 model.Request 交给 openaicompat.Provider，
// 再把模型回复转回 LoopStepResponse（支持 JSON 与 NDJSON 两种响应格式）。
type refLoop struct {
	opts refLoopOptions

	mu           sync.Mutex
	cache        map[string]*v1.LoopStepResponse
	sessionState map[string][]byte
}

func newRefLoop(opts refLoopOptions) *refLoop {
	return &refLoop{
		opts:         opts,
		cache:        map[string]*v1.LoopStepResponse{},
		sessionState: map[string][]byte{},
	}
}

var (
	refMarshal   = protojson.MarshalOptions{}
	refUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

func (l *refLoop) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req v1.LoopStepRequest
	if err := refUnmarshal.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad protojson", http.StatusBadRequest)
		return
	}

	if l.opts.cacheByRunTurn {
		l.mu.Lock()
		cached := l.cache[runTurnKey(&req)]
		l.mu.Unlock()
		if cached != nil {
			l.writeResponse(w, cached)
			return
		}
	}

	// 读取上一步的状态；正常实现只看 req.State，坏变体 sessionScopedState 会在 state 为空时回退到进程内按 session_id 保存的状态。
	rawState := req.GetState()
	if len(rawState) == 0 && l.opts.sessionScopedState {
		l.mu.Lock()
		rawState = l.sessionState[req.GetSessionId()]
		l.mu.Unlock()
	}
	st := refLoopState{RunID: req.GetRunId()}
	if len(rawState) > 0 {
		_ = json.Unmarshal(rawState, &st)
	}
	st.Steps++
	if l.opts.messagesInState {
		st.Messages = req.GetMessages()
	}

	mreq := buildModelRequest(&req, l.opts)
	prov := &openaicompat.Provider{BaseURL: l.opts.modelURL, APIKey: req.GetStepToken()}
	if l.opts.dropStepToken {
		prov.APIKey = "static-key"
	}

	// NDJSON 模式下先缓存流式增量或延迟写响应头，保证模型返回 500 时仍能返回非 200 状态码。
	headersWritten := false
	flusher, _ := w.(http.Flusher)
	var onDelta func(model.Delta)
	if l.opts.stream && l.opts.earlyHeaders {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		headersWritten = true
	}
	if l.opts.stream {
		onDelta = func(d model.Delta) {
			if d.Text == "" && d.Reasoning == "" {
				return
			}
			if !headersWritten {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				headersWritten = true
			}
			line, _ := refMarshal.Marshal(&v1.LoopStreamLine{
				Line: &v1.LoopStreamLine_Delta{Delta: &v1.LoopDelta{Text: d.Text, Reasoning: d.Reasoning}},
			})
			_, _ = w.Write(append(line, '\n'))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}

	mresp, err := prov.Generate(r.Context(), mreq, onDelta)
	if err != nil {
		if l.opts.okOnModelFailure {
			l.writeResponse(w, &v1.LoopStepResponse{})
			return
		}
		if !headersWritten {
			http.Error(w, "model error", http.StatusBadGateway)
		}
		return
	}

	nextState, _ := json.Marshal(st)
	if l.opts.sessionScopedState {
		l.mu.Lock()
		l.sessionState[req.GetSessionId()] = nextState
		l.mu.Unlock()
	}

	tcs := mresp.ToolCalls
	if l.opts.onlyFirstCall && len(tcs) > 1 {
		tcs = tcs[:1]
	}
	out := &v1.LoopStepResponse{
		Content:      mresp.Content,
		ToolCalls:    tcs,
		State:        nextState,
		StateVersion: "v1",
		Reasoning:    mresp.Reasoning,
	}

	if l.opts.cacheByRunTurn {
		l.mu.Lock()
		l.cache[runTurnKey(&req)] = out
		l.mu.Unlock()
	}

	if l.opts.stream {
		if !headersWritten {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
		}
		line, _ := refMarshal.Marshal(&v1.LoopStreamLine{
			Line: &v1.LoopStreamLine_Response{Response: out},
		})
		_, _ = w.Write(append(line, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	l.writeResponse(w, out)
}

// runTurnKey 是坏变体 cacheByRunTurn 的缓存键：只看 (run_id, turn)，忽略 attempt 与 messages。
func runTurnKey(req *v1.LoopStepRequest) string {
	return fmt.Sprintf("%s#%d", req.GetRunId(), req.GetTurn())
}

func (l *refLoop) writeResponse(w http.ResponseWriter, out *v1.LoopStepResponse) {
	b, _ := refMarshal.Marshal(out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func buildModelRequest(req *v1.LoopStepRequest, opts refLoopOptions) *model.Request {
	sys := req.GetSystem()
	if opts.dropSystem {
		sys = ""
	}
	msgs := make([]model.Message, 0, len(req.GetMessages()))
	for _, m := range req.GetMessages() {
		msgs = append(msgs, model.Message{
			Role:       model.Role(m.GetRole()),
			Content:    m.GetContent(),
			ToolCalls:  m.GetToolCalls(),
			ToolCallID: m.GetToolCallId(),
		})
	}
	if opts.dropAfterTool {
		lastTool := -1
		for i, m := range msgs {
			if m.Role == model.RoleTool {
				lastTool = i
			}
		}
		if lastTool >= 0 {
			msgs = msgs[:lastTool+1]
		}
	}
	if opts.reverseToolResults {
		// 找到最后一条带调用的助手消息，将其后工具消息的 ToolCallID 逆序配对。
		lastAsst := -1
		for i, m := range msgs {
			if m.Role == model.RoleAssistant && len(m.ToolCalls) > 0 {
				lastAsst = i
			}
		}
		if lastAsst >= 0 {
			var idxs []int
			for i := lastAsst + 1; i < len(msgs); i++ {
				if msgs[i].Role == model.RoleTool {
					idxs = append(idxs, i)
				}
			}
			for i, j := 0, len(idxs)-1; i < j; i, j = i+1, j-1 {
				msgs[idxs[i]].ToolCallID, msgs[idxs[j]].ToolCallID = msgs[idxs[j]].ToolCallID, msgs[idxs[i]].ToolCallID
			}
		}
	}

	tools := make([]model.ToolSpec, 0, len(req.GetTools()))
	for _, t := range req.GetTools() {
		tools = append(tools, model.ToolSpec{
			Name:        t.GetName(),
			Description: t.GetDescription(),
			InputSchema: json.RawMessage(t.GetInputSchemaJson()),
		})
	}
	return &model.Request{
		Model:    "looptest-ref",
		System:   sys,
		Messages: msgs,
		Tools:    tools,
	}
}

// restartableHandler 在测试中模拟进程重启：每次 Restart 都替换为全新的处理器实例。
type restartableHandler struct {
	cur atomic.Pointer[http.Handler]
}

func (h *restartableHandler) set(next http.Handler) { h.cur.Store(&next) }
func (h *restartableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*h.cur.Load()).ServeHTTP(w, r)
}

var testTokenKey = &extloop.TokenKey{KID: "looptest", Secret: strings.Repeat("k", 32)}

func TestReferenceLoopPassesAllScenarios(t *testing.T) {
	for _, tc := range []struct {
		name           string
		stream         bool
		earlyHeaders   bool
		wantStreamNote string
	}{
		{name: "json", stream: false, wantStreamNote: "non-streaming"},
		{name: "ndjson", stream: true, wantStreamNote: "streaming (application/x-ndjson"},
		{name: "ndjson-early-headers", stream: true, earlyHeaders: true, wantStreamNote: "streaming (application/x-ndjson"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm := NewModel()
			modelSrv := httptest.NewServer(fm)
			t.Cleanup(modelSrv.Close)

			rh := &restartableHandler{}
			makeLoop := func() http.Handler {
				return newRefLoop(refLoopOptions{modelURL: modelSrv.URL + "/v1", stream: tc.stream, earlyHeaders: tc.earlyHeaders})
			}
			rh.set(makeLoop())
			loopSrv := httptest.NewServer(rh)
			t.Cleanup(loopSrv.Close)

			restarted := 0
			cfg := Config{
				URL:      loopSrv.URL,
				Timeout:  10 * time.Second,
				Model:    fm,
				TokenKey: testTokenKey,
				Restart: func(context.Context) error {
					restarted++
					rh.set(makeLoop())
					return nil
				},
			}
			results := Run(t.Context(), cfg)
			if len(results) != len(ScenarioNames()) {
				t.Fatalf("got %d results, want %d", len(results), len(ScenarioNames()))
			}
			for _, res := range results {
				if res.Status != StatusPass {
					t.Errorf("scenario %s: status %s, reason: %s", res.Name, res.Status, res.Reason)
				}
				if res.Name == "streaming" && !strings.Contains(res.Reason, tc.wantStreamNote) {
					t.Errorf("streaming reason %q does not contain %q", res.Reason, tc.wantStreamNote)
				}
			}
			if restarted != 1 {
				t.Fatalf("restart hook called %d times, want 1", restarted)
			}
		})
	}
}

func TestReferenceLoopRunT(t *testing.T) {
	fm := NewModel()
	modelSrv := httptest.NewServer(fm)
	t.Cleanup(modelSrv.Close)

	rh := &restartableHandler{}
	rh.set(newRefLoop(refLoopOptions{modelURL: modelSrv.URL + "/v1", stream: true}))
	loopSrv := httptest.NewServer(rh)
	t.Cleanup(loopSrv.Close)

	RunT(t, Config{
		URL:     loopSrv.URL,
		Timeout: 10 * time.Second,
		Model:   fm,
		Restart: func(context.Context) error {
			rh.set(newRefLoop(refLoopOptions{modelURL: modelSrv.URL + "/v1", stream: true}))
			return nil
		},
	})
}

func TestBadVariantsFailExpectedScenarios(t *testing.T) {
	allNames := ScenarioNames()
	cases := []struct {
		name     string
		mutate   func(*refLoopOptions)
		wantFail []string
		// runFilter 缩小单测范围以加速；空表示跑全部场景。
		runFilter []string
	}{
		{
			name:      "only-first-call",
			mutate:    func(o *refLoopOptions) { o.onlyFirstCall = true },
			wantFail:  []string{"parallel-calls"},
			runFilter: []string{"parallel-calls"},
		},
		{
			name:      "cache-by-run-turn",
			mutate:    func(o *refLoopOptions) { o.cacheByRunTurn = true },
			wantFail:  []string{"repeat-step", "repeat-with-new-input"},
			runFilter: []string{"repeat-step", "repeat-with-new-input"},
		},
		{
			name:     "drop-system",
			mutate:   func(o *refLoopOptions) { o.dropSystem = true },
			wantFail: allNames,
		},
		{
			name:      "reverse-tool-results",
			mutate:    func(o *refLoopOptions) { o.reverseToolResults = true },
			wantFail:  []string{"parallel-calls"},
			runFilter: []string{"parallel-calls"},
		},
		{
			name:      "drop-after-tool-result",
			mutate:    func(o *refLoopOptions) { o.dropAfterTool = true },
			wantFail:  []string{"interjection"},
			runFilter: []string{"interjection"},
		},
		{
			name:      "ok-on-model-failure",
			mutate:    func(o *refLoopOptions) { o.okOnModelFailure = true },
			wantFail:  []string{"model-failure"},
			runFilter: []string{"model-failure"},
		},
		{
			name:      "messages-in-state",
			mutate:    func(o *refLoopOptions) { o.messagesInState = true },
			wantFail:  []string{"state-limit"},
			runFilter: []string{"state-limit"},
		},
		{
			name:      "drop-step-token",
			mutate:    func(o *refLoopOptions) { o.dropStepToken = true },
			wantFail:  []string{"single-step", "streaming"},
			runFilter: []string{"single-step", "streaming"},
		},
		{
			name:      "session-scoped-state",
			mutate:    func(o *refLoopOptions) { o.sessionScopedState = true },
			wantFail:  []string{"new-run-empty-state"},
			runFilter: []string{"new-run-empty-state"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm := NewModel()
			modelSrv := httptest.NewServer(fm)
			t.Cleanup(modelSrv.Close)

			rh := &restartableHandler{}
			makeLoop := func() http.Handler {
				opts := refLoopOptions{modelURL: modelSrv.URL + "/v1"}
				tc.mutate(&opts)
				return newRefLoop(opts)
			}
			rh.set(makeLoop())
			loopSrv := httptest.NewServer(rh)
			t.Cleanup(loopSrv.Close)

			results := Run(t.Context(), Config{
				URL:      loopSrv.URL,
				Timeout:  10 * time.Second,
				Model:    fm,
				Run:      tc.runFilter,
				TokenKey: testTokenKey,
				Restart: func(context.Context) error {
					rh.set(makeLoop())
					return nil
				},
			})
			byName := map[string]Result{}
			for _, res := range results {
				byName[res.Name] = res
			}
			for _, want := range tc.wantFail {
				res, ok := byName[want]
				if !ok {
					t.Errorf("scenario %s did not run", want)
					continue
				}
				if res.Status != StatusFail {
					t.Errorf("scenario %s: got %s (%s), want FAIL", want, res.Status, res.Reason)
				}
			}
		})
	}
}

func TestRestartSkippedWhenNil(t *testing.T) {
	fm := NewModel()
	modelSrv := httptest.NewServer(fm)
	t.Cleanup(modelSrv.Close)
	loopSrv := httptest.NewServer(newRefLoop(refLoopOptions{modelURL: modelSrv.URL + "/v1"}))
	t.Cleanup(loopSrv.Close)

	results := Run(t.Context(), Config{
		URL:   loopSrv.URL,
		Model: fm,
		Run:   []string{"restart"},
	})
	if len(results) != 1 || results[0].Status != StatusSkip {
		t.Fatalf("got %+v, want single SKIP", results)
	}
}

func TestModelNonStreamingAndContentParts(t *testing.T) {
	fm := NewModel()
	fm.beginScenario("custom", func(call ModelCall) ModelReply {
		if !strings.Contains(call.Messages[1].Content, "part-a") || !strings.Contains(call.Messages[1].Content, "part-b") {
			return ModelReply{Status: http.StatusBadRequest}
		}
		return ModelReply{
			ToolCalls: []ModelToolCall{{Name: "lookup", Arguments: `{"key":"v"}`}},
		}
	})
	defer fm.endScenario("custom")

	srv := httptest.NewServer(fm)
	t.Cleanup(srv.Close)

	// 不带 /v1 的路径、非流式、分块数组形式的 content 都须被假模型接受。
	reqBody := `{
		"model": "ignored",
		"stream": false,
		"messages": [
			{"role": "developer", "content": [{"type":"text","text":"looptest:custom"}]},
			{"role": "user", "content": [{"type":"text","text":"part-a"},{"type":"text","text":"part-b"}]}
		],
		"tools": [{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
	}`
	resp, err := http.Post(srv.URL+"/chat/completions", "application/json", bytes.NewBufferString(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string `json:"role"`
				Content   any    `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].FinishReason != "tool_calls" ||
		len(out.Choices[0].Message.ToolCalls) != 1 ||
		!strings.HasPrefix(out.Choices[0].Message.ToolCalls[0].ID, "fm_") ||
		out.Choices[0].Message.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if calls := fm.Calls("custom"); len(calls) != 1 || !slices.ContainsFunc(calls[0].Tools, func(mt ModelTool) bool { return mt.Name == "lookup" }) {
		t.Fatalf("recorded calls: %+v", calls)
	}
}
