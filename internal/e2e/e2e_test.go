// Package e2e 在真实 HTTP/WebSocket 传输上验证 M1 的端到端流程：
// 设备离线时 Run 挂起、设备上线后恢复，以及高风险调用的审批。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/node/wsgateway"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	sandboxdocker "yanshi/internal/sandbox/docker"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
	"yanshi/internal/workqueue/pgqueue"
	"yanshi/sdk/nodesdk"
)

// script 是脚本化模型：用户消息形如 "call <工具名后缀> [JSON 参数]"，即调用名称以该后缀结尾的工具；
// 收到工具结果后回复 "done: <结果>"。
type script struct{}

func (script) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == model.RoleTool {
		return &model.Response{Content: model.TextBlocks("done: " + model.Text(last.Content))}, nil
	}
	rest, _ := strings.CutPrefix(model.Text(last.Content), "call ")
	suffix, args, _ := strings.Cut(rest, " ")
	if args == "" {
		args = "{}"
	}
	for _, t := range req.Tools {
		if strings.HasSuffix(t.Name, suffix) {
			return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "x", Capability: t.Name, ArgumentsJson: args}}}, nil
		}
	}
	return &model.Response{Content: model.TextBlocks("no such tool")}, nil
}

type env struct {
	t      *testing.T
	srv    *httptest.Server
	writes atomic.Int32
}

// stores 是一个实例使用的存储；多个实例共享同一套 PostgreSQL 存储即构成分布式部署。
type stores struct {
	log   eventlog.Log
	queue workqueue.Queue
	dir   node.Directory
	inbox node.Inbox

	sandboxQueue workqueue.Queue
	ledger       nodesdk.Ledger
	activity     sandbox.Activity
	// provider 为 nil 时不启动沙箱控制器。
	provider sandbox.Provider
}

func memStores() stores {
	clk := clock.Real{}
	return stores{
		log: memlog.New(), queue: memqueue.New(clk), dir: node.NewMemDirectory(clk), inbox: node.NewMemInbox(),
		sandboxQueue: memqueue.New(clk), ledger: nodesdk.NewMemLedger(), activity: sandbox.NewMemActivity(),
	}
}

// instance 启动一个 yanshi 实例：workers 个 Worker，serve 为 true 时提供 HTTP API 与 Node 网关。
func instance(t *testing.T, st stores, workers int, serve bool) *httptest.Server {
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "dev", Version: "1", Model: "script/any", Capabilities: []string{"device:*", "sandbox:*"}})
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.Real{}
	store := &session.Store{Log: st.log, IDs: ids.Random(), Clock: clk}
	bus := live.NewMemBus()
	hub := &node.Hub{Dir: st.dir, Inbox: st.inbox, Store: store, Queue: st.queue, Auth: node.InsecureDevAuth{}}
	catalog := &capability.Catalog{Local: capability.NewRegistry(), Nodes: st.dir, DefaultTimeout: time.Minute,
		Sandbox: &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID}}
	router := &sandbox.Router{Hub: hub, Queue: st.sandboxQueue}
	gw := model.NewGateway()
	gw.Register("script", script{})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for i := range workers {
		w := runtime.New(runtime.Config{ID: fmt.Sprintf("w%d", i), Store: store, Queue: st.queue, Agents: agents, Model: gw,
			Catalog: catalog, Dispatch: router, Live: bus, IdleWait: 5 * time.Millisecond})
		go w.Run(ctx)
	}
	if st.provider != nil {
		c := &sandbox.Controller{ID: "c1", Queue: st.sandboxQueue, Hub: hub, Provider: st.provider, Activity: st.activity,
			Ledger: st.ledger, Clock: clk, IdleWait: 5 * time.Millisecond}
		go c.Run(ctx)
	}
	if !serve {
		return nil
	}
	svc := &service.Service{Store: store, Queue: st.queue, Agents: agents, Nodes: router}
	mux := http.NewServeMux()
	mux.Handle("/v1/nodes/connect", &wsgateway.Gateway{Hub: hub})
	mux.Handle("/", (&httpapi.Server{Service: svc, Live: bus, Nodes: st.dir}).Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T) *env {
	return &env{t: t, srv: instance(t, memStores(), 2, true)}
}

// startNode 启动一个 SDK 节点并等待其上线；返回停止函数。
func (e *env) startNode(ledger nodesdk.Ledger) func() {
	e.t.Helper()
	text := func(s string) []*v1.ContentBlock {
		return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
	}
	exec := nodesdk.NewExecutor(ledger,
		nodesdk.Capability{
			Spec:    &v1.CapabilitySpec{Name: "read_file", Idempotent: true, Risk: v1.Risk_RISK_LOW},
			Handler: func(context.Context, string) ([]*v1.ContentBlock, error) { return text("meeting notes"), nil },
		},
		nodesdk.Capability{
			Spec: &v1.CapabilitySpec{Name: "write_file", Risk: v1.Risk_RISK_HIGH},
			Handler: func(context.Context, string) ([]*v1.ContentBlock, error) {
				e.writes.Add(1)
				return text("written"), nil
			},
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	connected := make(chan struct{}, 1)
	c := nodesdk.NewClient(nodesdk.Config{
		URL: "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/nodes/connect", NodeID: "node-1",
		BusinessLine: "bl", EndUser: "u", Label: "MacBook", Kind: "desktop", Executor: exec,
		OnConnected: func(string) { connected <- struct{}{} },
	})
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		e.t.Fatal("node did not connect")
	}
	return func() { cancel(); <-done }
}

func (e *env) do(method, path string, body any, out any) int {
	e.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

type sessionView struct {
	Runs []struct {
		RunID   string `json:"run_id"`
		Status  string `json:"status"`
		Waiting *struct {
			Kind       string `json:"kind"`
			CallID     string `json:"call_id"`
			Capability string `json:"capability"`
			NodeOnline *bool  `json:"node_online"`
		} `json:"waiting"`
	} `json:"runs"`
}

// until 轮询 Session 直到 cond 成立。
func (e *env) until(sid string, cond func(sessionView) bool) sessionView {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v sessionView
		e.do(http.MethodGet, "/v1/sessions/"+sid, nil, &v)
		if cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("condition not met; last view %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastRun(v sessionView) (status string, ok bool) {
	if len(v.Runs) == 0 {
		return "", false
	}
	return v.Runs[len(v.Runs)-1].Status, true
}

func (e *env) newSession() string {
	var res struct {
		SessionID string `json:"session_id"`
	}
	e.do(http.MethodPost, "/v1/sessions", map[string]string{"business_line": "bl", "end_user": "u", "agent": "dev"}, &res)
	return res.SessionID
}

func (e *env) lastAssistantText(sid string) string {
	var res struct {
		Events []json.RawMessage `json:"events"`
	}
	e.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil, &res)
	text := ""
	for _, raw := range res.Events {
		var ev struct {
			AssistantMessage *struct {
				Content []struct {
					Text struct {
						Text string `json:"text"`
					} `json:"text"`
				} `json:"content"`
			} `json:"assistant_message"`
		}
		_ = json.Unmarshal(raw, &ev)
		if ev.AssistantMessage != nil && len(ev.AssistantMessage.Content) > 0 {
			text = ev.AssistantMessage.Content[0].Text.Text
		}
	}
	return text
}

func TestOfflineDeviceSuspendsAndResumes(t *testing.T) {
	e := setup(t)
	ledger := nodesdk.NewMemLedger()
	stop := e.startNode(ledger)
	stop() // 设备曾经上线（目录里有它），现在离线

	sid := e.newSession()
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "call macbook__read_file"}, nil)
	v := e.until(sid, func(v sessionView) bool {
		st, ok := lastRun(v)
		return ok && st == "suspended" && v.Runs[0].Waiting != nil
	})
	w := v.Runs[0].Waiting
	if w.Kind != "node" || w.Capability != "macbook__read_file" || w.NodeOnline == nil || *w.NodeOnline {
		t.Fatalf("waiting = %+v", w)
	}

	defer e.startNode(ledger)() // 设备上线，Inbox 中的调用被投递
	e.until(sid, func(v sessionView) bool { st, _ := lastRun(v); return st == "completed" })
	if got := e.lastAssistantText(sid); got != "done: meeting notes" {
		t.Fatalf("assistant = %q", got)
	}
}

func TestHighRiskCallRequiresApproval(t *testing.T) {
	e := setup(t)
	defer e.startNode(nodesdk.NewMemLedger())()

	for _, approve := range []bool{false, true} {
		sid := e.newSession()
		e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "call macbook__write_file"}, nil)
		v := e.until(sid, func(v sessionView) bool {
			return len(v.Runs) > 0 && v.Runs[0].Waiting != nil && v.Runs[0].Waiting.Kind == "approval"
		})
		if e.writes.Load() != 0 && !approve {
			t.Fatal("write executed before approval")
		}
		call := v.Runs[0].Waiting.CallID
		if code := e.do(http.MethodPost, "/v1/sessions/"+sid+"/approvals/"+call, map[string]bool{"approve": approve}, nil); code != http.StatusNoContent {
			t.Fatalf("decide: %d", code)
		}
		e.until(sid, func(v sessionView) bool { st, _ := lastRun(v); return st == "completed" })
		text := e.lastAssistantText(sid)
		if approve && (text != "done: written" || e.writes.Load() != 1) {
			t.Fatalf("approved: text %q writes %d", text, e.writes.Load())
		}
		if !approve && (!strings.Contains(text, "denied") || e.writes.Load() != 0) {
			t.Fatalf("denied: text %q writes %d", text, e.writes.Load())
		}
		if code := e.do(http.MethodPost, "/v1/sessions/"+sid+"/approvals/"+call, map[string]bool{"approve": true}, nil); code != http.StatusNotFound && code != http.StatusConflict {
			t.Fatalf("second decision: %d", code)
		}
	}
}

// TestCrossInstanceOnPostgres：设备连在实例 A（只提供 API 与网关，没有 Worker），
// Run 由实例 B（只有 Worker）执行。调用经 PostgreSQL 中的 Inbox 与 LISTEN/NOTIFY 跨实例投递。
func TestCrossInstanceOnPostgres(t *testing.T) {
	pool, notifier := pgtest.Fresh(t)
	clk := clock.Real{}
	shared := func() stores {
		return stores{
			log: pglog.New(pool, notifier), queue: pgqueue.New(pool, clk, pgqueue.Sessions),
			dir: pgnode.NewDirectory(pool, clk), inbox: pgnode.NewInbox(pool, notifier),
			sandboxQueue: pgqueue.New(pool, clk, pgqueue.Sandboxes),
			ledger:       pgsandbox.Ledger{Pool: pool}, activity: pgsandbox.Activity{Pool: pool},
		}
	}
	e := &env{t: t, srv: instance(t, shared(), 0, true)}
	instance(t, shared(), 2, false)
	defer e.startNode(nodesdk.NewMemLedger())()

	sid := e.newSession()
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "call macbook__read_file"}, nil)
	e.until(sid, func(v sessionView) bool { st, _ := lastRun(v); return st == "completed" })
	if got := e.lastAssistantText(sid); got != "done: meeting notes" {
		t.Fatalf("assistant = %q", got)
	}
}

// TestSandboxInDocker 走完整链路：HTTP → Worker → 沙箱队列 → 控制器 → 真实 Docker 沙箱。
// 两次调用在不同的 Run 中执行，验证工作区跨 Run 保留。
func TestSandboxInDocker(t *testing.T) {
	if os.Getenv("YANSHI_TEST_DOCKER") == "" {
		t.Skip("YANSHI_TEST_DOCKER not set")
	}
	st := memStores()
	p := &sandboxdocker.Provider{Image: "python:3.12-slim", MemoryMB: 256}
	st.provider = p
	e := &env{t: t, srv: instance(t, st, 2, true)}
	sid := e.newSession()
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sandbox.NodeID(sid)) })

	steps := []struct{ input, want string }{
		{`call __run_python {"code":"open('n.txt','w').write(str(6*7))"}`, "exit_code: 0"},
		{`call __exec {"command":"cat n.txt && id -u"}`, "42"},
	}
	for i, step := range steps {
		e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": step.input}, nil)
		e.until(sid, func(v sessionView) bool {
			return len(v.Runs) == i+1 && v.Runs[i].Status == "completed"
		})
		if got := e.lastAssistantText(sid); !strings.Contains(got, step.want) {
			t.Fatalf("step %d: assistant = %q, want containing %q", i, got, step.want)
		}
	}
	if got := e.lastAssistantText(sid); !strings.Contains(got, "1000") {
		t.Fatalf("sandbox did not run as uid 1000: %q", got)
	}
}
