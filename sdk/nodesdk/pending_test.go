package nodesdk

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

// fakeGateway 记录客户端发出的消息，并把网关的消息交给客户端（不经网络）。
type fakeGateway struct {
	t    *testing.T
	c    *Client
	mu   sync.Mutex
	sent []*v1.NodeMessage
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{t: t, c: NewClient(Config{})}
	err := g.c.sessions.attach(func(_ context.Context, m *v1.NodeMessage) error {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.sent = append(g.sent, m)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *fakeGateway) push(e *v1.Event) {
	g.c.sessions.handle(&v1.GatewayMessage{Msg: &v1.GatewayMessage_Event{Event: e}})
}

// subs 返回客户端发出的订阅与退订：["+c1@0", "-c1"]。
func (g *fakeGateway) subs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, m := range g.sent {
		switch {
		case m.GetSubscribe() != nil:
			out = append(out, fmt.Sprintf("+%s@%d", m.GetSubscribe().GetSessionId(), m.GetSubscribe().GetAfterSeq()))
		case m.GetUnsubscribe() != nil:
			out = append(out, "-"+m.GetUnsubscribe().GetSessionId())
		}
	}
	return out
}

func (g *fakeGateway) waitSubs(want ...string) {
	g.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Equal(g.subs(), want) {
		if time.Now().After(deadline) {
			g.t.Fatalf("subscriptions %v, want %v", g.subs(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// eventLog 生成一个 Session 的事件，seq 依次递增。
type eventLog struct {
	sid string
	seq uint64
}

func (l *eventLog) ev(e *v1.Event) *v1.Event {
	l.seq++
	e.SessionId, e.Seq = l.sid, l.seq
	return e
}

func (l *eventLog) calls(runID string, calls ...[3]string) *v1.Event {
	m := &v1.AssistantMessage{RunId: runID, Attempt: 1}
	for _, c := range calls {
		args := c[2]
		if args == "" {
			args = "{}"
		}
		m.ToolCalls = append(m.ToolCalls, &v1.ToolCall{CallId: c[0], Capability: c[1], ArgumentsJson: args})
	}
	return l.ev(&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: m}})
}

// asked：ask_user 派发给用户本人，此后才算待回答。
func (l *eventLog) asked(runID, callID string) *v1.Event {
	return l.ev(&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{
		RunId: runID, Attempt: 1, CallId: callID, NodeId: "@user"}}})
}

func (l *eventLog) delegate(runID, callID, child string) *v1.Event {
	return l.ev(&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{
		RunId: runID, Attempt: 1, CallId: callID, NodeId: "@agent/" + child}}})
}

func (l *eventLog) result(runID, callID string) *v1.Event {
	return l.ev(&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: runID, CallId: callID}}})
}

func (l *eventLog) approval(runID, callID, summary string) *v1.Event {
	return l.ev(&v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{
		RunId: runID, Attempt: 1, CallId: callID, Summary: summary}}})
}

func (l *eventLog) completed(runID string) *v1.Event {
	return l.ev(&v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: runID, Attempt: 1}}})
}

func askArgs(q string) string {
	return fmt.Sprintf(`{"question":%q,"options":[{"id":"a","label":"A"}]}`, q)
}

func brief(items []PendingItem) string {
	var out []string
	for _, i := range items {
		text := i.Summary
		if i.Kind == PendingQuestion {
			text = i.Question.Question
		}
		out = append(out, fmt.Sprintf("%s:%s/%s:%s:%s:%s", i.Kind, i.SessionID, i.CallID, i.Agent, i.Via, text))
	}
	return strings.Join(out, " ")
}

// changes 收集 onChange 的调用。
type changes struct {
	mu   sync.Mutex
	last []PendingItem
	n    int
}

func (c *changes) on(items []PendingItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last, c.n = items, c.n+1
}

func (c *changes) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		got, n := brief(c.last), c.n
		c.mu.Unlock()
		if n > 0 && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("onChange saw %q, want %q", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPendingFollowsDelegations(t *testing.T) {
	g := newFakeGateway(t)
	var ch changes
	tr := NewPendingTracker(g.c, "top", ch.on)
	defer tr.Close()
	top, child, grand := &eventLog{sid: "top"}, &eventLog{sid: "c1"}, &eventLog{sid: "g1"}

	tr.OnEvent(top.calls("r1", [3]string{"d1", "agent__planner", `{"task":"x"}`}))
	tr.OnEvent(top.delegate("r1", "d1", "c1"))
	g.waitSubs("+c1@0")

	g.push(child.calls("cr1", [3]string{"q1", "ask_user", askArgs("去哪？")}))
	g.push(child.asked("cr1", "q1"))
	g.push(child.calls("cr1", [3]string{"w1", "phone__send_sms"}))
	g.push(child.approval("cr1", "w1", "发短信"))
	ch.wait(t, "approval:c1/w1:planner:c1:发短信 question:c1/q1:planner:c1:去哪？")
	if a := tr.Items()[0]; a.Capability != "phone__send_sms" || a.RunID != "cr1" {
		t.Fatalf("approval %+v", a)
	}

	// 孙 Session：归到顶层的直接子 Session 名下。
	g.push(child.result("cr1", "q1"))
	g.push(child.calls("cr1", [3]string{"d2", "agent__helper"}))
	g.push(child.delegate("cr1", "d2", "g1"))
	g.waitSubs("+c1@0", "+g1@0")
	g.push(grand.calls("gr1", [3]string{"q2", "ask_user", askArgs("几点？")}))
	g.push(grand.asked("gr1", "q2"))
	ch.wait(t, "approval:c1/w1:planner:c1:发短信 question:g1/q2:helper:c1:几点？")

	// 子 Run 结束：它的委托随之结束，孙 Session 退订，待处理项收起。
	g.push(child.completed("cr1"))
	g.waitSubs("+c1@0", "+g1@0", "-g1")
	ch.wait(t, "")

	// 委托调用的结果：退订子 Session；退订之后迟到的子 Session 事件不再影响列表。
	tr.OnEvent(top.result("r1", "d1"))
	g.waitSubs("+c1@0", "+g1@0", "-g1", "-c1")
	g.push(child.calls("cr1", [3]string{"q3", "ask_user", askArgs("还有？")}))
	g.push(child.asked("cr1", "q3"))
	if items := tr.Items(); len(items) != 0 {
		t.Fatalf("items after the delegation ended: %s", brief(items))
	}

	// 父 Run 结束同样结束委托；Close 取消其余子订阅。
	c2 := &eventLog{sid: "c2"}
	tr.OnEvent(top.calls("r2", [3]string{"d3", "agent__helper"}))
	tr.OnEvent(top.delegate("r2", "d3", "c2"))
	g.waitSubs("+c1@0", "+g1@0", "-g1", "-c1", "+c2@0")
	g.push(c2.calls("cr2", [3]string{"q4", "ask_user", askArgs("确定？")}))
	g.push(c2.asked("cr2", "q4"))
	ch.wait(t, "question:c2/q4:helper:c2:确定？")
	tr.OnEvent(top.ev(&v1.Event{Payload: &v1.Event_RunInterrupted{RunInterrupted: &v1.RunInterrupted{RunId: "r2"}}}))
	g.waitSubs("+c1@0", "+g1@0", "-g1", "-c1", "+c2@0", "-c2")
	ch.wait(t, "")

	tr.OnEvent(top.calls("r3", [3]string{"d4", "agent__helper"}))
	tr.OnEvent(top.delegate("r3", "d4", "c3"))
	g.waitSubs("+c1@0", "+g1@0", "-g1", "-c1", "+c2@0", "-c2", "+c3@0")
	tr.Close()
	g.waitSubs("+c1@0", "+g1@0", "-g1", "-c1", "+c2@0", "-c2", "+c3@0", "-c3")
}

func TestPendingApprovalsClose(t *testing.T) {
	g := newFakeGateway(t)
	tr := NewPendingTracker(g.c, "top", nil)
	defer tr.Close()
	top := &eventLog{sid: "top"}
	tr.OnEvent(top.calls("r1", [3]string{"w1", "mac__write_file"}, [3]string{"w2", "mac__write_file"}))
	tr.OnEvent(top.approval("r1", "w1", "写 1"))
	tr.OnEvent(top.approval("r1", "w2", "写 2"))
	if got := brief(tr.Items()); got != "approval:top/w1:::写 1 approval:top/w2:::写 2" {
		t.Fatal(got)
	}
	tr.OnEvent(top.ev(&v1.Event{Payload: &v1.Event_ApprovalDecided{ApprovalDecided: &v1.ApprovalDecided{RunId: "r1", CallId: "w1", Approved: true}}}))
	tr.OnEvent(top.result("r1", "w2"))
	tr.OnEvent(top.calls("r2", [3]string{"w3", "mac__write_file"}))
	tr.OnEvent(top.approval("r2", "w3", "写 3"))
	tr.OnEvent(top.completed("r2"))
	if items := tr.Items(); len(items) != 0 {
		t.Fatal(brief(items))
	}
	// 其他 Session 的事件忽略。
	tr.OnEvent((&eventLog{sid: "other"}).calls("r9", [3]string{"q", "ask_user", askArgs("?")}))
	if items := tr.Items(); len(items) != 0 {
		t.Fatal(brief(items))
	}
}

func TestPendingTypedAnswerTarget(t *testing.T) {
	g := newFakeGateway(t)
	var ch changes
	tr := NewPendingTracker(g.c, "top", ch.on)
	defer tr.Close()
	top, child := &eventLog{sid: "top"}, &eventLog{sid: "c1"}
	target := func() string {
		if q := tr.TypedAnswerTarget(); q != nil {
			return q.SessionID + "/" + q.CallID
		}
		return ""
	}
	if got := target(); got != "" {
		t.Fatalf("no question: %q", got)
	}

	// 恰好一个提问，在子 Session 中：选中它；审批不算提问。
	tr.OnEvent(top.calls("r1", [3]string{"d1", "agent__helper"}))
	tr.OnEvent(top.delegate("r1", "d1", "c1"))
	g.waitSubs("+c1@0")
	g.push(child.calls("cr1", [3]string{"q1", "ask_user", askArgs("去哪？")}))
	g.push(child.asked("cr1", "q1"))
	g.push(child.calls("cr1", [3]string{"w1", "phone__send_sms"}))
	g.push(child.approval("cr1", "w1", "发短信"))
	if got := target(); got != "c1/q1" {
		t.Fatalf("one sub-agent question: %q", got)
	}

	// 两个待回答的提问：不选中。
	g.push(child.calls("cr1", [3]string{"q2", "ask_user", askArgs("几点？")}))
	g.push(child.asked("cr1", "q2"))
	if got := target(); got != "" {
		t.Fatalf("two questions: %q", got)
	}
	g.push(child.result("cr1", "q2"))

	// 顶层 Session 自己有提问：不选中子 Session 的。
	tr.OnEvent(top.calls("r1", [3]string{"q3", "ask_user", askArgs("要继续吗？")}))
	tr.OnEvent(top.asked("r1", "q3"))
	if got := target(); got != "" {
		t.Fatalf("top-level question: %q", got)
	}
	tr.OnEvent(top.result("r1", "q3"))
	if got := target(); got != "c1/q1" {
		t.Fatalf("after the top-level answer: %q", got)
	}

	// 只有顶层自己的提问：不选中。
	tr.OnEvent(top.result("r1", "d1"))
	tr.OnEvent(top.calls("r1", [3]string{"q4", "ask_user", askArgs("好吗？")}))
	tr.OnEvent(top.asked("r1", "q4"))
	if got, n := target(), len(tr.Items()); got != "" || n != 1 {
		t.Fatalf("only a top-level question: %q (%d items)", got, n)
	}
}

// 提问派发给用户（ToolCallStarted{node_id: "@user"}）之后才算待回答：模型同一轮返回委托与提问时，Worker 先推进委托。
func TestPendingQuestionOpensWhenDispatched(t *testing.T) {
	g := newFakeGateway(t)
	var ch changes
	tr := NewPendingTracker(g.c, "top", ch.on)
	defer tr.Close()
	top, child := &eventLog{sid: "top"}, &eventLog{sid: "c1"}
	tr.OnEvent(top.calls("r1", [3]string{"d1", "agent__helper"}, [3]string{"q0", "ask_user", askArgs("要订酒店吗？")}))
	tr.OnEvent(top.delegate("r1", "d1", "c1"))
	g.waitSubs("+c1@0")
	g.push(child.calls("cr1", [3]string{"q1", "ask_user", askArgs("去哪？")}))
	if items := tr.Items(); len(items) != 0 {
		t.Fatalf("undispatched questions are open: %s", brief(items))
	}
	g.push(child.asked("cr1", "q1"))
	ch.wait(t, "question:c1/q1:helper:c1:去哪？")
	if q := tr.TypedAnswerTarget(); q == nil || q.CallID != "q1" {
		t.Fatalf("the parent's undispatched question blocks the sub-agent's: %+v", q)
	}

	tr.OnEvent(top.result("r1", "d1"))
	tr.OnEvent(top.asked("r1", "q0"))
	ch.wait(t, "question:top/q0:::要订酒店吗？")
	if q := tr.TypedAnswerTarget(); q != nil {
		t.Fatalf("top-level question: %+v", q)
	}
	// 派发到别处的 ToolCallStarted 不算派发给用户。
	tr.OnEvent(top.calls("r1", [3]string{"q9", "ask_user", askArgs("?")}))
	tr.OnEvent(top.ev(&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: "r1", CallId: "q9", NodeId: "phone"}}}))
	if n := len(tr.Items()); n != 1 {
		t.Fatalf("items %s", brief(tr.Items()))
	}
}

// 子 Session 的订阅以 not_found 或 error 结束而委托仍在进行时，退避后重订；deleted、委托结束或 Close 之后不再重订。
func TestPendingRetriesChildSubscription(t *testing.T) {
	retryMin, retryMax = 10*time.Millisecond, 40*time.Millisecond
	defer func() { retryMin, retryMax = 200*time.Millisecond, 5*time.Second }()
	g := newFakeGateway(t)
	var ch changes
	tr := NewPendingTracker(g.c, "top", ch.on)
	defer tr.Close()
	top, child := &eventLog{sid: "top"}, &eventLog{sid: "c1"}
	ended := func(sid, reason string) {
		g.c.sessions.handle(&v1.GatewayMessage{Msg: &v1.GatewayMessage_SubscriptionEnded{SubscriptionEnded: &v1.SubscriptionEnded{SessionId: sid, Reason: reason}}})
	}
	tr.OnEvent(top.calls("r1", [3]string{"d1", "agent__helper"}))
	tr.OnEvent(top.delegate("r1", "d1", "c1"))
	g.waitSubs("+c1@0")
	// 子 Session 还没创建。
	ended("c1", "not_found")
	g.waitSubs("+c1@0", "+c1@0")
	g.push(child.calls("cr1", [3]string{"q1", "ask_user", askArgs("去哪？")}))
	g.push(child.asked("cr1", "q1"))
	ch.wait(t, "question:c1/q1:helper:c1:去哪？")

	// 网关一侧的错误：保留待处理项，从已收到的位置续订。
	ended("c1", "error")
	if n := len(tr.Items()); n != 1 {
		t.Fatalf("items while retrying: %s", brief(tr.Items()))
	}
	g.waitSubs("+c1@0", "+c1@0", "+c1@2")

	// deleted：收起，不再重订。
	ended("c1", "deleted")
	ch.wait(t, "")
	time.Sleep(60 * time.Millisecond)
	g.waitSubs("+c1@0", "+c1@0", "+c1@2")

	// 委托在退避期间结束：不再重订。
	tr.OnEvent(top.calls("r2", [3]string{"d2", "agent__helper"}))
	tr.OnEvent(top.delegate("r2", "d2", "c2"))
	g.waitSubs("+c1@0", "+c1@0", "+c1@2", "+c2@0")
	ended("c2", "not_found")
	tr.OnEvent(top.result("r2", "d2"))
	time.Sleep(60 * time.Millisecond)
	g.waitSubs("+c1@0", "+c1@0", "+c1@2", "+c2@0")

	// Close 之后不再重订。
	tr.OnEvent(top.calls("r3", [3]string{"d3", "agent__helper"}))
	tr.OnEvent(top.delegate("r3", "d3", "c3"))
	g.waitSubs("+c1@0", "+c1@0", "+c1@2", "+c2@0", "+c3@0")
	ended("c3", "not_found")
	tr.Close()
	time.Sleep(60 * time.Millisecond)
	g.waitSubs("+c1@0", "+c1@0", "+c1@2", "+c2@0", "+c3@0")
}

// 跟踪器对子 Session 的订阅与 HostApp 自己的订阅并存：应用打开子 Session 不挤掉跟踪器，各自取消。
func TestObserverCoexistsWithSubscriber(t *testing.T) {
	g := newFakeGateway(t)
	var app, helper eventRecorder
	offApp := g.c.Subscribe("s1", 2, &app)
	g.c.SetActivity("s1", true, false)
	l := &eventLog{sid: "s1", seq: 2}
	g.push(l.completed("r1")) // seq 3
	offHelper := g.c.observe("s1", 0, &helper)
	if got := g.subs(); !slices.Equal(got, []string{"+s1@2", "+s1@0"}) {
		t.Fatalf("subscriptions %v", got)
	}
	replay := &eventLog{sid: "s1"}
	for range 4 {
		g.push(replay.completed("r1")) // seq 1..4
	}
	if a, h := app.seqs(), helper.seqs(); !slices.Equal(a, []uint64{3, 4}) || !slices.Equal(h, []uint64{1, 2, 3, 4}) {
		t.Fatalf("app %v, helper %v", a, h)
	}

	// 应用取消：网关上不退订，但撤下前台状态；之后的 SetActivity 忽略。
	g.mu.Lock()
	before := len(g.sent)
	g.mu.Unlock()
	offApp()
	g.c.SetActivity("s1", true, false)
	g.push(replay.completed("r1")) // seq 5
	g.mu.Lock()
	after := g.sent[before:]
	g.mu.Unlock()
	if len(after) != 1 || after[0].GetActivity() == nil || after[0].GetActivity().GetFocused() {
		t.Fatalf("after the app unsubscribed: %v", after)
	}
	if a, h := app.seqs(), helper.seqs(); len(a) != 2 || h[len(h)-1] != 5 {
		t.Fatalf("app %v, helper %v", a, h)
	}
	offHelper()
	if got := g.subs(); got[len(got)-1] != "-s1" {
		t.Fatalf("subscriptions %v", got)
	}

	// 网关结束订阅时，所有订阅者都收到通知。
	g.c.Subscribe("s2", 0, &app)
	g.c.observe("s2", 0, &helper)
	g.c.sessions.handle(&v1.GatewayMessage{Msg: &v1.GatewayMessage_SubscriptionEnded{SubscriptionEnded: &v1.SubscriptionEnded{SessionId: "s2", Reason: "deleted"}}})
	if app.ended != "deleted" || helper.ended != "deleted" {
		t.Fatalf("ended: %q %q", app.ended, helper.ended)
	}
}

type eventRecorder struct {
	events []*v1.Event
	ended  string
}

func (r *eventRecorder) OnEvent(e *v1.Event)   { r.events = append(r.events, e) }
func (r *eventRecorder) OnDelta(*v1.LiveDelta) {}
func (r *eventRecorder) OnEnded(reason string) { r.ended = reason }

func (r *eventRecorder) seqs() []uint64 {
	var out []uint64
	for _, e := range r.events {
		out = append(out, e.GetSeq())
	}
	return out
}
