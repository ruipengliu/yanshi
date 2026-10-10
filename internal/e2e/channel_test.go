package e2e

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/memory/pgmemory"
	"yanshi/internal/model"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/notify"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/presence/pgpresence"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/workqueue/pgqueue"
	"yanshi/sdk/nodesdk"
)

// recorder 记录一个订阅收到的消息。
type recorder struct {
	mu     sync.Mutex
	events []*v1.Event
	deltas []*v1.LiveDelta
	ended  string
}

func (r *recorder) OnEvent(e *v1.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) OnDelta(d *v1.LiveDelta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltas = append(r.deltas, d)
}

func (r *recorder) OnEnded(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = reason
}

func (r *recorder) snapshot() ([]*v1.Event, []*v1.LiveDelta, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*v1.Event(nil), r.events...), append([]*v1.LiveDelta(nil), r.deltas...), r.ended
}

// completed 报告是否已收到 n 个 RunCompleted。
func (r *recorder) completed(n int) bool {
	events, _, _ := r.snapshot()
	got := 0
	for _, e := range events {
		if e.GetRunCompleted() != nil {
			got++
		}
	}
	return got >= n
}

func (e *env) connURL() string { return "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/connect" }

// startClient 启动一个只作会话客户端的连接（不登记为 Node）。
func (e *env) startClient(id string, tokens nodesdk.TokenSource, onConnected func()) *nodesdk.Client {
	e.t.Helper()
	connected := make(chan struct{}, 8)
	c := nodesdk.NewClient(nodesdk.Config{URL: e.connURL(), NodeID: id, TokenSource: tokens, Label: id, Kind: "phone",
		OnConnected: func(string) {
			if onConnected != nil {
				onConnected()
			}
			connected <- struct{}{}
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		e.t.Fatal("client did not connect")
	}
	return c
}

// TestDuplexChannelStreamsToEveryDevice：两台设备经各自的连接订阅同一 Session。一台创建 Session 并提交输入，
// 两台都收到已提交事件；生成中途才订阅的设备先收到一条 Snapshot（到目前为止的全文），再接着收到增量。
func TestDuplexChannelStreamsToEveryDevice(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	phone := e.startClient("phone", userToken, nil)
	sid, err := phone.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var onPhone recorder
	phone.Subscribe(sid, 0, &onPhone)
	runID, steered, err := phone.SubmitText(ctx, sid, "stream 40")
	if err != nil || steered || runID == "" {
		t.Fatalf("submit: run=%q steered=%v err=%v", runID, steered, err)
	}
	eventually(t, "phone sees deltas", 5*time.Second, func() bool {
		_, deltas, _ := onPhone.snapshot()
		return len(deltas) >= 5
	})

	pc := e.startClient("pc", userToken, nil)
	var onPC recorder
	pc.Subscribe(sid, 0, &onPC)
	eventually(t, "both devices see the run complete", 10*time.Second, func() bool { return onPhone.completed(1) && onPC.completed(1) })

	_, pcDeltas, _ := onPC.snapshot()
	if len(pcDeltas) == 0 || !pcDeltas[0].GetSnapshot() || !strings.HasPrefix(pcDeltas[0].GetText(), "chunk0 chunk1 ") {
		t.Fatalf("late joiner's first delta should be a snapshot of the draft so far, got %v", pcDeltas)
	}
	// 草稿 = Snapshot + 之后的增量，应恰好是完整的流式输出（不漏、不重）。
	var draft strings.Builder
	for _, d := range pcDeltas {
		if d.GetSnapshot() {
			draft.Reset()
		}
		draft.WriteString(d.GetText())
	}
	var want strings.Builder
	for i := range 40 {
		want.WriteString("chunk" + strconv.Itoa(i) + " ")
	}
	if draft.String() != want.String() {
		t.Fatalf("late joiner's draft = %q, want %q", draft.String(), want.String())
	}
	if last := pcDeltas[len(pcDeltas)-1]; !last.GetEnd() {
		t.Fatalf("generation should end with an end marker, got %v", last)
	}
	for _, r := range []*recorder{&onPhone, &onPC} {
		events, _, _ := r.snapshot()
		for i, ev := range events {
			if ev.GetSeq() != uint64(i+1) {
				t.Fatalf("events should be contiguous from seq 1, got seq %d at %d", ev.GetSeq(), i)
			}
			if a := ev.GetAttemptStarted(); a != nil && a.GetLiveEndpoint() != "" {
				t.Fatal("internal live endpoint leaked to a client")
			}
		}
	}
}

// TestNodeAndClientShareOneConnection：同一条连接既是 Node（提供高风险 Capability），又是会话客户端：
// 它看到审批请求、经同一连接批准，然后执行调用。
func TestNodeAndClientShareOneConnection(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	exec := nodesdk.NewExecutor(nodesdk.NewMemLedger(), nodesdk.Capability{
		Spec: &v1.CapabilitySpec{Name: "write_file", Risk: v1.Risk_RISK_HIGH},
		Handler: func(context.Context, string) ([]*v1.ContentBlock, error) {
			e.writes.Add(1)
			return model.TextBlocks("written"), nil
		},
	})
	connected := make(chan struct{}, 1)
	c := nodesdk.NewClient(nodesdk.Config{URL: e.connURL(), NodeID: "mac", TokenSource: userToken, Label: "MacBook",
		Kind: "desktop", Executor: exec, OnConnected: func(string) { connected <- struct{}{} }})
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = c.Run(cctx) }()
	<-connected

	sid, err := c.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var rec recorder
	c.Subscribe(sid, 0, &rec)
	if _, _, err := c.SubmitText(ctx, sid, "call macbook__write_file"); err != nil {
		t.Fatal(err)
	}
	var callID string
	eventually(t, "approval requested", 5*time.Second, func() bool {
		events, _, _ := rec.snapshot()
		for _, ev := range events {
			if a := ev.GetApprovalRequested(); a != nil {
				callID = a.GetCallId()
			}
		}
		return callID != ""
	})
	if err := c.Decide(ctx, sid, callID, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "run completes after approval", 5*time.Second, func() bool { return rec.completed(1) })
	if e.writes.Load() != 1 {
		t.Fatalf("write_file ran %d times", e.writes.Load())
	}
	// 再次决定同一调用：冲突，错误码与 HTTP 409 对应。
	err = c.Decide(ctx, sid, callID, true)
	if re, ok := err.(*nodesdk.RequestError); !ok || re.Code != "not_found" && re.Code != "conflict" {
		t.Fatalf("deciding a finished call: %v", err)
	}
}

// TestChannelOnlyReachesOwnSessions：会话客户端只能访问自己的 Session，其余一律视为不存在。
func TestChannelOnlyReachesOwnSessions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	other := func(context.Context) (string, error) {
		return blKey.Sign("yanshi", "someone-else", time.Hour, time.Now())
	}
	mine := e.startClient("mine", userToken, nil)
	theirs := e.startClient("theirs", other, nil)
	sid, err := mine.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var rec recorder
	theirs.Subscribe(sid, 0, &rec)
	eventually(t, "foreign subscription ends", 5*time.Second, func() bool {
		_, _, ended := rec.snapshot()
		return ended == "not_found"
	})
	for name, err := range map[string]error{
		"submit":    func() error { _, _, err := theirs.SubmitText(ctx, sid, "hi"); return err }(),
		"close":     theirs.CloseSession(ctx, sid, ""),
		"interrupt": theirs.Interrupt(ctx, sid, "run_x"),
	} {
		if re, ok := err.(*nodesdk.RequestError); !ok || re.Code != "not_found" {
			t.Errorf("%s on a foreign session: %v", name, err)
		}
	}
}

func mustSign(user string) string {
	tok, err := blKey.Sign("yanshi", user, time.Hour, time.Now())
	if err != nil {
		panic(err)
	}
	return tok
}

// TestChannelTextFrames：以文本帧发 Hello 的连接按 protojson 收发，网页无需 protobuf 运行时也能接入。
func TestChannelTextFrames(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, e.connURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	send := func(m *v1.NodeMessage) {
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	read := func() *v1.GatewayMessage {
		typ, b, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != websocket.MessageText {
			t.Fatal("reply to a text-frame connection should be a text frame")
		}
		m := &v1.GatewayMessage{}
		if err := protojson.Unmarshal(b, m); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return m
	}
	send(&v1.NodeMessage{Msg: &v1.NodeMessage_Hello{Hello: &v1.Hello{NodeId: "web-1", Token: mustSign("u"), ClientOnly: true}}})
	if read().GetWelcome() == nil {
		t.Fatal("expected welcome")
	}
	send(&v1.NodeMessage{Msg: &v1.NodeMessage_Request{Request: &v1.ClientRequest{RequestId: "a",
		Op: &v1.ClientRequest_CreateSession{CreateSession: &v1.CreateSession{Agent: "dev"}}}}})
	resp := read().GetResponse()
	if resp.GetRequestId() != "a" || resp.GetSessionId() == "" || resp.GetError() != nil {
		t.Fatalf("create session response = %v", resp)
	}
	send(&v1.NodeMessage{Msg: &v1.NodeMessage_Subscribe{Subscribe: &v1.Subscribe{SessionId: resp.GetSessionId()}}})
	// 在场列表与事件交错到达。
	var ev *v1.Event
	for ev == nil {
		m := read()
		if p := m.GetPresence(); p != nil && (len(p.GetViewers()) != 1 || p.GetViewers()[0].GetDeviceId() != "web-1") {
			t.Fatalf("presence = %v", p)
		}
		ev = m.GetEvent()
	}
	if ev.GetSessionCreated() == nil {
		t.Fatalf("first event should be SessionCreated, got %v", ev)
	}
	// 客户端连接没有登记为 Node。
	var nodes struct{ Nodes []map[string]any }
	e.doAs(mustSign("u"), "GET", "/v1/nodes", nil, &nodes)
	if len(nodes.Nodes) != 0 {
		t.Fatalf("a client-only connection was registered as a node: %v", nodes.Nodes)
	}
}

// TestChannelResumesAfterReconnect：令牌到期断开后，SDK 重连并按最后收到的 seq 续订，事件不漏、不重。
func TestChannelResumesAfterReconnect(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	short := func(context.Context) (string, error) { return blKey.Sign("yanshi", "u", 2*time.Second, time.Now()) }
	var connects atomic.Int32
	c := e.startClient("phone", short, func() { connects.Add(1) })
	sid, err := c.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var rec recorder
	c.Subscribe(sid, 0, &rec)
	if _, _, err := c.SubmitText(ctx, sid, "hello"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first run completes", 5*time.Second, func() bool { return rec.completed(1) })
	eventually(t, "reconnect after the token expires", 8*time.Second, func() bool { return connects.Load() >= 2 })
	if _, _, err := c.SubmitText(ctx, sid, "again"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second run completes", 5*time.Second, func() bool { return rec.completed(2) })
	events, _, _ := rec.snapshot()
	for i, ev := range events {
		if ev.GetSeq() != uint64(i+1) {
			t.Fatalf("events across the reconnect should be contiguous, got seq %d at %d", ev.GetSeq(), i)
		}
	}
}

// presenceRecorder 额外记录在场列表。
type presenceRecorder struct {
	recorder
	pmu  sync.Mutex
	last *v1.Presence
}

func (r *presenceRecorder) OnPresence(p *v1.Presence) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	r.last = p
}

// viewers 返回最近一次在场列表的摘要，如 "iphone:focused,typing macbook:"。
func (r *presenceRecorder) viewers() string {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	var parts []string
	for _, v := range r.last.GetViewers() {
		var flags []string
		if v.GetFocused() {
			flags = append(flags, "focused")
		}
		if v.GetTyping() {
			flags = append(flags, "typing")
		}
		parts = append(parts, v.GetLabel()+":"+strings.Join(flags, ","))
	}
	return strings.Join(parts, " ")
}

// TestPresenceAcrossInstances：两台设备连在共享 PostgreSQL 的两个实例上，各自看到对方是否在看、
// 是否聚焦、是否正在输入；一台离开后另一台随即看到。
func TestPresenceAcrossInstances(t *testing.T) {
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
			memories: pgmemory.Store{Pool: pool}, grants: pgmemory.Grants{Pool: pool},
			presence: pgpresence.Store{Pool: pool, Notifier: notifier}, push: notify.NewMemRegistry(),
		}
	}
	a := &env{t: t, srv: instance(t, shared(), 1, true)}
	b := &env{t: t, srv: instance(t, shared(), 0, true)}
	ctx := context.Background()

	phone := a.startClient("iphone", userToken, nil)
	sid, err := phone.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var onPhone presenceRecorder
	phone.Subscribe(sid, 0, &onPhone)
	eventually(t, "phone sees itself", 5*time.Second, func() bool { return onPhone.viewers() == "iphone:" })

	mac := b.startClient("macbook", userToken, nil)
	var onMac presenceRecorder
	mac.Subscribe(sid, 0, &onMac)
	eventually(t, "both see both", 5*time.Second, func() bool {
		return onPhone.viewers() == "iphone: macbook:" && onMac.viewers() == "iphone: macbook:"
	})

	phone.SetActivity(sid, true, true)
	eventually(t, "mac sees the phone focused and typing", 5*time.Second, func() bool {
		return onMac.viewers() == "iphone:focused,typing macbook:"
	})
	phone.SetActivity(sid, true, false)
	eventually(t, "typing stops", 5*time.Second, func() bool { return onMac.viewers() == "iphone:focused macbook:" })

	// 设备对它没有订阅的 Session 报告状态：忽略。
	phone.SetActivity("ses_other", true, true)

	// 退订：另一台设备随即看到它离开。
	cancel := mac.Subscribe(sid, 0, &onMac)
	cancel()
	eventually(t, "phone sees the mac leave", 5*time.Second, func() bool { return onPhone.viewers() == "iphone:focused" })
}

const roomQuestion = `{"question":"订哪个会议室？","options":[{"id":"a","label":"A201"},{"id":"b","label":"B302"}]}`

// pendingQuestion 返回事件中最近一次 ask_user 调用的 ID（若尚无结果）。
func pendingQuestionID(events []*v1.Event) string {
	id := ""
	for _, ev := range events {
		for _, tc := range ev.GetAssistantMessage().GetToolCalls() {
			if tc.GetCapability() == "ask_user" {
				id = tc.GetCallId()
			}
		}
		if ev.GetToolResult().GetCallId() == id {
			id = ""
		}
	}
	return id
}

// TestAskUserAcrossDevices：Agent 提问后 Run 挂起；手机看到问题，电脑上点选回答，回答作为调用结果
// 回到 Run（模型看到选中项的标签）；两台设备都看到结果。再次回答同一问题是冲突。
func TestAskUserAcrossDevices(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	phone, pc := e.startClient("phone", userToken, nil), e.startClient("pc", userToken, nil)
	sid, err := phone.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var onPhone, onPC recorder
	phone.Subscribe(sid, 0, &onPhone)
	pc.Subscribe(sid, 0, &onPC)
	if _, _, err := phone.SubmitText(ctx, sid, "call ask_user "+roomQuestion); err != nil {
		t.Fatal(err)
	}
	var callID string
	eventually(t, "question reaches the pc", 5*time.Second, func() bool {
		events, _, _ := onPC.snapshot()
		callID = pendingQuestionID(events)
		return callID != ""
	})
	var view sessionView
	e.until(sid, func(v sessionView) bool {
		view = v
		return len(v.Runs) > 0 && v.Runs[0].Waiting != nil && v.Runs[0].Waiting.Kind == "question"
	})
	if view.Runs[0].Waiting.CallID != callID {
		t.Fatalf("waiting on %s, want question %s", view.Runs[0].Waiting.CallID, callID)
	}

	// 不合法的回答被拒绝，不影响提问。
	err = pc.Answer(ctx, sid, callID, []string{"c"}, nil, "")
	if re, ok := err.(*nodesdk.RequestError); !ok || re.Code != "invalid" {
		t.Fatalf("answering with an unknown option: %v", err)
	}
	if err := pc.Answer(ctx, sid, callID, []string{"b"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both see the run complete", 5*time.Second, func() bool { return onPhone.completed(1) && onPC.completed(1) })
	if got := e.lastAssistantText(sid); !strings.Contains(got, `"label":"B302"`) {
		t.Fatalf("model saw %q, want the selected label", got)
	}
	err = phone.Answer(ctx, sid, callID, []string{"a"}, nil, "")
	if re, ok := err.(*nodesdk.RequestError); !ok || re.Code != "conflict" {
		t.Fatalf("answering twice: %v", err)
	}
}

// TestTypingAnswersTheQuestion：Run 等用户回答时，用户直接打字就是在回答（而不是插话）。
func TestTypingAnswersTheQuestion(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.startClient("phone", userToken, nil)
	sid, err := c.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var rec recorder
	c.Subscribe(sid, 0, &rec)
	runID, _, err := c.SubmitText(ctx, sid, "call ask_user "+roomQuestion)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "question asked", 5*time.Second, func() bool {
		events, _, _ := rec.snapshot()
		return pendingQuestionID(events) != ""
	})
	again, steered, err := c.SubmitText(ctx, sid, "都不行，换周五")
	if err != nil || !steered || again != runID {
		t.Fatalf("typed answer: run %s steered %v err %v", again, steered, err)
	}
	eventually(t, "run completes", 5*time.Second, func() bool { return rec.completed(1) })
	events, _, _ := rec.snapshot()
	for _, ev := range events {
		if ev.GetSteered() != nil {
			t.Fatal("typed answer was recorded as a steer")
		}
	}
	if got := e.lastAssistantText(sid); !strings.Contains(got, `"text":"都不行，换周五"`) {
		t.Fatalf("model saw %q", got)
	}
}

// TestRetryAfterLostResponseAppliesOnce：提交后连接立即断开（结果未知），另一条连接以同一输入 ID 重试：
// 输入恰好生效一次；再重试一次得到首次的结果（duplicate）。
func TestRetryAfterLostResponseAppliesOnce(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := e.startClient("phone", userToken, nil)
	sid, err := c.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	in := &v1.SubmitInput{SessionId: sid, Input: model.TextBlocks("hello"), InputId: nodesdk.NewInputID()}

	ws, _, err := websocket.Dial(ctx, e.connURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []*v1.NodeMessage{
		{Msg: &v1.NodeMessage_Hello{Hello: &v1.Hello{NodeId: "flaky", Token: mustSign("u"), ClientOnly: true}}},
		{Msg: &v1.NodeMessage_Request{Request: &v1.ClientRequest{RequestId: "r", Op: &v1.ClientRequest_Submit{Submit: in}}}},
	} {
		b, _ := protojson.Marshal(m)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	ws.CloseNow() // 不等响应

	first, err := c.SubmitInput(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.SubmitInput(ctx, in)
	if err != nil || !again.GetDuplicate() || again.GetRunId() != first.GetRunId() {
		t.Fatalf("second retry: %v %v (first %v)", again, err, first)
	}
	var rec recorder
	c.Subscribe(sid, 0, &rec)
	eventually(t, "run completes", 5*time.Second, func() bool { return rec.completed(1) })
	events, _, _ := rec.snapshot()
	n := 0
	for _, ev := range events {
		if ev.GetRunRequested().GetInputId() == in.GetInputId() || ev.GetSteered().GetInputId() == in.GetInputId() {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("input applied %d times", n)
	}
}

// recPusher 记录推送。
type recPusher struct {
	mu   sync.Mutex
	msgs []string // "设备/种类"
}

func (p *recPusher) Push(_ context.Context, d *notify.Device, m notify.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, d.DeviceID+"/"+string(m.Kind))
	return nil
}

func (p *recPusher) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.msgs...)
}

// TestNotifiesWhenNobodyIsWatching：手机登记了推送。它没在看时，待审批推送给它；它把 Session 显示在前台时不推送。
func TestNotifiesWhenNobodyIsWatching(t *testing.T) {
	st := memStores()
	push := &recPusher{}
	st.pusher = push
	e := &env{t: t, srv: instance(t, st, 2, true), disk: map[string][]byte{}}
	defer e.startNode(nodesdk.NewMemLedger())()
	ctx := context.Background()
	connected := make(chan struct{}, 1)
	phone := nodesdk.NewClient(nodesdk.Config{URL: e.connURL(), NodeID: "iphone", TokenSource: userToken, Label: "iphone", Kind: "phone",
		PushPlatform: "apns", PushToken: func() string { return "apns-token" }, OnConnected: func(string) { connected <- struct{}{} }})
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = phone.Run(cctx) }()
	<-connected

	sid, err := phone.CreateSession(ctx, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	var rec recorder
	phone.Subscribe(sid, 0, &rec)
	if _, _, err := phone.SubmitText(ctx, sid, "call macbook__write_file"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "approval pushed to the phone", 5*time.Second, func() bool {
		return slices.Contains(push.got(), "iphone/approval")
	})
	approve := func() {
		t.Helper()
		var callID string
		eventually(t, "approval requested", 5*time.Second, func() bool {
			events, _, _ := rec.snapshot()
			callID = ""
			for _, ev := range events {
				if a := ev.GetApprovalRequested(); a != nil {
					callID = a.GetCallId()
				}
				if d := ev.GetApprovalDecided(); d != nil && d.GetCallId() == callID {
					callID = ""
				}
			}
			return callID != ""
		})
		if err := phone.Decide(ctx, sid, callID, true); err != nil {
			t.Fatal(err)
		}
	}
	approve()
	eventually(t, "first run completes", 5*time.Second, func() bool { return rec.completed(1) })

	// 手机把这个 Session 显示在前台：再次审批不推送。
	phone.SetActivity(sid, true, false)
	time.Sleep(100 * time.Millisecond)
	if _, _, err := phone.SubmitText(ctx, sid, "call macbook__write_file"); err != nil {
		t.Fatal(err)
	}
	approve()
	eventually(t, "second run completes", 5*time.Second, func() bool { return rec.completed(2) })
	if n := len(push.got()); n != 1 {
		t.Fatalf("pushes %v, want only the first approval", push.got())
	}
}
