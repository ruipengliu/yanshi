// Package e2e 在真实 HTTP/WebSocket 传输上验证 M1 的端到端流程：
// 设备离线时 Run 挂起、设备上线后恢复，以及高风险调用的审批。
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/auth"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/live"
	"yanshi/internal/live/peer"
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
// 收到工具结果后回复 "done: <结果>"。"stream <n>" 则在约 n×20ms 内逐段流式输出，用于观察实时增量。
type script struct{}

func (script) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == model.RoleTool {
		return &model.Response{Content: model.TextBlocks("done: " + model.Text(last.Content))}, nil
	}
	if n, ok := strings.CutPrefix(model.Text(last.Content), "stream "); ok {
		count, _ := strconv.Atoi(n)
		for i := range count {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			if onDelta != nil {
				onDelta(model.Delta{Text: fmt.Sprintf("chunk%d ", i)})
			}
		}
		return &model.Response{Content: model.TextBlocks("streamed")}, nil
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
	// disk 模拟设备上的文件。
	diskMu sync.Mutex
	disk   map[string][]byte
}

// e2e 全程开启鉴权：业务线 "bl" 的开发密钥扮演业务线服务端，为 EndUser "u" 签发令牌。
var (
	blKey     = mustKey()
	verifier  = mustVerifier(blKey)
	userToken = func(context.Context) (string, error) { return blKey.Sign("yanshi", "u", time.Hour, time.Now()) }
)

func mustKey() *auth.SigningKey {
	k, err := auth.GenerateDevKey("bl", "bl-1")
	if err != nil {
		panic(err)
	}
	return k
}

func mustVerifier(k *auth.SigningKey) *auth.JWT {
	pub, err := k.Public()
	if err != nil {
		panic(err)
	}
	v, err := auth.NewJWT("yanshi", clock.Real{}, pub)
	if err != nil {
		panic(err)
	}
	return v
}

func bearer(req *http.Request) {
	t, _ := userToken(req.Context())
	req.Header.Set("Authorization", "Bearer "+t)
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
	provider  sandbox.Provider
	artifacts *artifact.Service

	index        lifecycle.Index
	deletions    lifecycle.Deletions
	janitorQueue workqueue.Queue
}

func memStores() stores {
	clk := clock.Real{}
	return stores{
		log: memlog.New(), queue: memqueue.New(clk), dir: node.NewMemDirectory(clk), inbox: node.NewMemInbox(),
		sandboxQueue: memqueue.New(clk), ledger: nodesdk.NewMemLedger(), activity: sandbox.NewMemActivity(),
		artifacts: &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: artifact.NewMemBlobs(), IDs: ids.Random(), Clock: clk},
		index:     lifecycle.NewMemIndex(), deletions: lifecycle.NewMemDeletions(), janitorQueue: memqueue.New(clk),
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
	// 每个实例都有内部增量接口，多实例共享存储时增量可跨实例送达（ADR-0013）。
	local := live.NewMemBus()
	peerMux := http.NewServeMux()
	peerMux.Handle(peer.Path, peer.Handler{Local: local, Token: "t"})
	peerSrv := httptest.NewServer(peerMux)
	t.Cleanup(peerSrv.Close)
	bus := &peer.Bus{Local: local, Log: st.log, Self: peerSrv.URL, Token: "t", Retry: 20 * time.Millisecond}
	st.artifacts.Deletions = st.deletions
	hub := &node.Hub{Dir: st.dir, Inbox: st.inbox, Store: store, Queue: st.queue, Auth: node.TokenAuth{Verifier: verifier}, Deletions: st.deletions}
	catalog := &capability.Catalog{Local: capability.NewRegistry(), Nodes: st.dir, DefaultTimeout: time.Minute,
		Sandbox: &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID}}
	router := &sandbox.Router{Hub: hub, Queue: st.sandboxQueue}
	gw := model.NewGateway()
	gw.Register("script", script{})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for i := range workers {
		w := runtime.New(runtime.Config{ID: fmt.Sprintf("w%d", i), Store: store, Queue: st.queue, Agents: agents, Model: gw,
			Catalog: catalog, Dispatch: router, Artifacts: st.artifacts, Live: bus, LiveEndpoint: peerSrv.URL, IdleWait: 5 * time.Millisecond})
		go w.Run(ctx)
	}
	if st.provider != nil {
		c := &sandbox.Controller{ID: "c1", Queue: st.sandboxQueue, Hub: hub, Provider: st.provider, Activity: st.activity,
			Ledger: st.ledger, Artifacts: st.artifacts, Clock: clk, IdleWait: 5 * time.Millisecond,
			Lifecycle: &lifecycle.Guard{Index: st.index, Deletions: st.deletions}}
		go c.Run(ctx)
	}
	svc := &service.Service{Store: store, Queue: st.queue, Agents: agents, Nodes: router,
		Index: st.index, Deletions: st.deletions, Janitor: st.janitorQueue}
	if workers > 0 {
		j := &janitor.Janitor{ID: "j1", Queue: st.janitorQueue, Service: svc, Store: store, Sessions: st.queue,
			SandboxQueue: st.sandboxQueue, Inbox: st.inbox, Nodes: st.dir, Sandbox: st.provider, Activity: st.activity,
			Ledger: st.ledger, Artifacts: st.artifacts, Index: st.index, Deletions: st.deletions, Clock: clk,
			IdleWait: 10 * time.Millisecond}
		go j.Run(ctx)
	}
	if !serve {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/nodes/connect", &wsgateway.Gateway{Hub: hub})
	mux.Handle("/", (&httpapi.Server{Auth: verifier, Service: svc, Live: bus, Nodes: st.dir, Artifacts: st.artifacts}).Handler())
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
	arts := &nodesdk.Artifacts{BaseURL: e.srv.URL, TokenSource: userToken}
	exec := nodesdk.NewExecutor(ledger,
		nodesdk.Capability{
			Spec: &v1.CapabilitySpec{Name: "upload_file", Idempotent: true, Risk: v1.Risk_RISK_LOW},
			Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
				var in struct{ Path string }
				_ = json.Unmarshal([]byte(args), &in)
				e.diskMu.Lock()
				data, ok := e.disk[in.Path]
				e.diskMu.Unlock()
				if !ok {
					return nil, fmt.Errorf("no such file %s", in.Path)
				}
				b, err := arts.Upload(ctx, nodesdk.Invocation(ctx).GetSessionId(), in.Path, "", bytes.NewReader(data))
				if err != nil {
					return nil, err
				}
				return []*v1.ContentBlock{b}, nil
			},
		},
		nodesdk.Capability{
			Spec: &v1.CapabilitySpec{Name: "save_artifact", Risk: v1.Risk_RISK_HIGH},
			Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
				var in struct{ Artifact, Path string }
				_ = json.Unmarshal([]byte(args), &in)
				body, _, sid, err := arts.Download(ctx, in.Artifact)
				if err != nil {
					return nil, err
				}
				defer body.Close()
				if sid != nodesdk.Invocation(ctx).GetSessionId() {
					return nil, fmt.Errorf("artifact belongs to another session")
				}
				data, err := io.ReadAll(body)
				if err != nil {
					return nil, err
				}
				e.diskMu.Lock()
				e.disk[in.Path] = data
				e.diskMu.Unlock()
				return text("saved"), nil
			},
		},
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
		TokenSource: userToken, Label: "MacBook", Kind: "desktop", Executor: exec,
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
	bearer(req)
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
	// 身份取自令牌，不再自报。
	e.do(http.MethodPost, "/v1/sessions", map[string]string{"agent": "dev"}, &res)
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
			artifacts: &artifact.Service{Meta: pgartifact.Meta{Pool: pool}, Blobs: artifact.NewMemBlobs(), IDs: ids.Random(), Clock: clk},
			index:     pglifecycle.Index{Pool: pool}, deletions: pglifecycle.Deletions{Pool: pool}, janitorQueue: pgqueue.New(pool, clk, pgqueue.Janitor),
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

	// API 实例没有 Worker：它的 SSE 收到的增量全部来自执行实例；内部地址不暴露给客户端。
	deltas, events := e.subscribe(sid)
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "stream 50"}, nil)
	select {
	case d := <-deltas:
		if !strings.HasPrefix(d.Text, "chunk") {
			t.Fatalf("delta = %+v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no live delta crossed instances")
	}
	e.until(sid, func(v sessionView) bool {
		st, _ := lastRun(v)
		return st == "completed" && e.lastAssistantText(sid) == "streamed"
	})
	for len(events) > 0 {
		if raw := <-events; strings.Contains(raw, "live_endpoint") {
			t.Fatalf("event leaks internal endpoint: %s", raw)
		}
	}
}

// subscribe 打开 Session 的 SSE 流，分别送出增量与事件原文。
func (e *env) subscribe(sid string) (<-chan live.Delta, chan string) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/v1/sessions/"+sid+"/stream", nil)
	bearer(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	deltas, events := make(chan live.Delta, 1024), make(chan string, 1024)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		kind := ""
		for sc.Scan() {
			line := sc.Text()
			if k, ok := strings.CutPrefix(line, "event: "); ok {
				kind = k
			}
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			if kind == "delta" {
				var d live.Delta
				if json.Unmarshal([]byte(data), &d) == nil {
					select {
					case deltas <- d:
					default:
					}
				}
			} else {
				select {
				case events <- data:
				default:
				}
			}
		}
	}()
	return deltas, events
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

// lastToolMedia 返回最近一个调用结果中引用的工件 ID。
func (e *env) lastToolMedia(sid string) string {
	var res struct {
		Events []json.RawMessage `json:"events"`
	}
	e.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil, &res)
	id := ""
	for _, raw := range res.Events {
		ev := &v1.Event{}
		if err := protojson.Unmarshal(raw, ev); err != nil {
			e.t.Fatal(err)
		}
		for _, c := range ev.GetToolResult().GetContent() {
			if a, ok := artifact.ParseURI(c.GetMedia().GetUri()); ok {
				id = a
			}
		}
	}
	return id
}

// TestScenarioADataPath 走通北极星场景 A 的数据通路（真实 Docker 沙箱）：
// 电脑上的文件 → 工件 → 沙箱处理 → 结果工件 → 经审批保存回电脑。
func TestScenarioADataPath(t *testing.T) {
	if os.Getenv("YANSHI_TEST_DOCKER") == "" {
		t.Skip("YANSHI_TEST_DOCKER not set")
	}
	st := memStores()
	p := &sandboxdocker.Provider{Image: "python:3.12-slim", MemoryMB: 256}
	st.provider = p
	e := &env{t: t, srv: instance(t, st, 2, true), disk: map[string][]byte{
		"meeting.csv": []byte("speaker,minutes\n张三,12\n李四,30\n张三,8\n"),
	}}
	defer e.startNode(nodesdk.NewMemLedger())()
	sid := e.newSession()
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sandbox.NodeID(sid)) })

	run := func(i int, input string) {
		t.Helper()
		e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": input}, nil)
		e.until(sid, func(v sessionView) bool {
			if len(v.Runs) == i+1 && v.Runs[i].Waiting != nil && v.Runs[i].Waiting.Kind == "approval" {
				e.do(http.MethodPost, "/v1/sessions/"+sid+"/approvals/"+v.Runs[i].Waiting.CallID, map[string]bool{"approve": true}, nil)
			}
			return len(v.Runs) == i+1 && v.Runs[i].Status == "completed"
		})
	}
	run(0, `call macbook__upload_file {"path":"meeting.csv"}`)
	in := e.lastToolMedia(sid)
	if in == "" {
		t.Fatal("device upload produced no artifact")
	}
	run(1, `call sandbox__import_file {"artifact":"`+in+`","path":"data/meeting.csv"}`)
	code := "import csv, collections\n" +
		"c = collections.Counter()\n" +
		"for r in csv.DictReader(open('data/meeting.csv')): c[r['speaker']] += int(r['minutes'])\n" +
		"open('summary.txt', 'w').write(chr(10).join(f'{k}: {v} 分钟' for k, v in c.most_common()))\n"
	args, _ := json.Marshal(map[string]string{"code": code})
	run(2, "call sandbox__run_python "+string(args))
	if got := e.lastAssistantText(sid); !strings.Contains(got, "exit_code: 0") {
		t.Fatalf("python failed: %s", got)
	}
	run(3, `call sandbox__export_file {"path":"summary.txt"}`)
	out := e.lastToolMedia(sid)
	if out == "" || out == in {
		t.Fatalf("export produced no new artifact (%q)", out)
	}
	run(4, `call macbook__save_artifact {"artifact":"`+out+`","path":"summary.txt"}`)

	e.diskMu.Lock()
	defer e.diskMu.Unlock()
	if got := string(e.disk["summary.txt"]); got != "李四: 30 分钟\n张三: 20 分钟" {
		t.Fatalf("summary on device = %q", got)
	}
}

// TestNodeReconnectsWhenTokenExpires：网关在令牌到期时断开 Node，SDK 通过 TokenSource 取新令牌重连。
func TestNodeReconnectsWhenTokenExpires(t *testing.T) {
	e := setup(t)
	var issued atomic.Int32
	short := func(context.Context) (string, error) {
		issued.Add(1)
		return blKey.Sign("yanshi", "u", 2*time.Second, time.Now())
	}
	connected := make(chan struct{}, 4)
	exec := nodesdk.NewExecutor(nodesdk.NewMemLedger())
	c := nodesdk.NewClient(nodesdk.Config{
		URL: "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/nodes/connect", NodeID: "node-exp",
		TokenSource: short, Label: "phone", Kind: "mobile", Executor: exec,
		OnConnected: func(string) { connected <- struct{}{} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	for i := range 2 {
		select {
		case <-connected:
		case <-time.After(8 * time.Second):
			t.Fatalf("connection %d never happened", i+1)
		}
	}
	if issued.Load() < 2 {
		t.Fatalf("reconnected without a fresh token (issued %d)", issued.Load())
	}

	// 令牌不属于 Hello 自报的身份时拒绝接入。
	other, _ := blKey.Sign("yanshi", "someone-else", time.Hour, time.Now())
	bad := nodesdk.NewClient(nodesdk.Config{
		URL: "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/nodes/connect", NodeID: "node-bad",
		Token: other, EndUser: "u", Label: "x", Executor: exec,
		OnConnected: func(string) { t.Error("node with mismatched identity was accepted") },
	})
	bctx, bcancel := context.WithTimeout(context.Background(), time.Second)
	defer bcancel()
	_ = bad.Run(bctx)
}
