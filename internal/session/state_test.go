package session

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/workqueue"
)

// 以下构造函数生成不带 seq 的事件，由 build 按顺序编号。
func created() *v1.Event {
	return &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}}
}
func requested(run string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: run}}}
}
func steered(run string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: run}}}
}
func attempt(run string, n uint32) *v1.Event {
	return &v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: run, Attempt: n}}}
}
func assistant(run string, n uint32, calls ...string) *v1.Event {
	m := &v1.AssistantMessage{RunId: run, Attempt: n}
	for _, c := range calls {
		m.ToolCalls = append(m.ToolCalls, &v1.ToolCall{CallId: c, Capability: "x"})
	}
	return &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: m}}
}
func started(run string, n uint32, call string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: run, Attempt: n, CallId: call}}}
}
func result(run string, n uint32, call string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: run, Attempt: n, CallId: call}}}
}
func completed(run string, n uint32) *v1.Event {
	return &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: run, Attempt: n}}}
}
func failed(run string, n uint32) *v1.Event {
	return &v1.Event{Payload: &v1.Event_RunFailed{RunFailed: &v1.RunFailed{RunId: run, Attempt: n}}}
}
func interrupted(run string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_RunInterrupted{RunInterrupted: &v1.RunInterrupted{RunId: run}}}
}

func build(events ...*v1.Event) []*v1.Event {
	for i, e := range events {
		e.SessionId, e.Seq = "s", uint64(i+1)
	}
	return events
}

func TestHappyPathWithToolCallAndTakeover(t *testing.T) {
	st, err := Reduce(build(
		created(),
		requested("r1"),
		attempt("r1", 1),
		assistant("r1", 1, "c1"),
		started("r1", 1, "c1"),
		steered("r1"),
		attempt("r1", 2), // 接管
		started("r1", 2, "c1"),
		result("r1", 2, "c1"),
		assistant("r1", 2),
		completed("r1", 2),
		requested("r2"),
	))
	if err != nil {
		t.Fatal(err)
	}
	r1 := st.Run("r1")
	if r1.Status != RunCompleted || r1.Attempt != 2 || r1.Turns != 2 {
		t.Fatalf("r1 = %+v", r1)
	}
	if got := r1.Calls[0].StartedAttempts; len(got) != 2 {
		t.Fatalf("started attempts = %v", got)
	}
	if a := st.Active(); a == nil || a.ID != "r2" || a.Status != RunQueued {
		t.Fatalf("active = %+v", a)
	}
	if len(st.History) != 6 {
		t.Fatalf("history has %d events, want 6", len(st.History))
	}
}

func TestRejectsInvalidTransitions(t *testing.T) {
	base := []*v1.Event{created(), requested("r1"), attempt("r1", 1)}
	cases := map[string]struct {
		tail []*v1.Event
		want string
	}{
		"second active run":          {[]*v1.Event{requested("r2")}, "is active"},
		"stale attempt event":        {[]*v1.Event{attempt("r1", 2), assistant("r1", 1)}, "stale attempt"},
		"attempt skips number":       {[]*v1.Event{attempt("r1", 3)}, "does not follow"},
		"event after terminal":       {[]*v1.Event{interrupted("r1"), steered("r1")}, "is interrupted"},
		"complete with pending":      {[]*v1.Event{assistant("r1", 1, "c1"), completed("r1", 1)}, "pending"},
		"assistant while pending":    {[]*v1.Event{assistant("r1", 1, "c1"), assistant("r1", 1)}, "pending"},
		"duplicate result":           {[]*v1.Event{assistant("r1", 1, "c1"), result("r1", 1, "c1"), result("r1", 1, "c1")}, "already done"},
		"started twice same attempt": {[]*v1.Event{assistant("r1", 1, "c1"), started("r1", 1, "c1"), started("r1", 1, "c1")}, "started twice"},
		"call id reused in session": {[]*v1.Event{assistant("r1", 1, "c1"), result("r1", 1, "c1"), assistant("r1", 1), completed("r1", 1),
			requested("r2"), attempt("r2", 1), assistant("r2", 1, "c1")}, "duplicate call id"},
		"fail queued run":   {[]*v1.Event{interrupted("r1"), requested("r2"), failed("r2", 0)}, "stale attempt"},
		"duplicate created": {[]*v1.Event{created()}, "duplicate"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			events := build(append(append([]*v1.Event{}, base...), c.tail...)...)
			_, err := Reduce(events)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestFirstEventMustBeCreated(t *testing.T) {
	if _, err := Reduce(build(requested("r1"))); err == nil {
		t.Fatal("want error")
	}
}

func TestSeqMustBeContiguous(t *testing.T) {
	events := build(created(), requested("r1"))
	events[1].Seq = 3
	if _, err := Reduce(events); err == nil || !strings.Contains(err.Error(), "seq gap") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckDoesNotMutate(t *testing.T) {
	st, err := Reduce(build(created(), requested("r1"), attempt("r1", 1)))
	if err != nil {
		t.Fatal(err)
	}
	e := assistant("r1", 1, "c1")
	e.SessionId = "s"
	if err := st.Check(e); err != nil {
		t.Fatal(err)
	}
	if st.Seq != 3 || len(st.Run("r1").Calls) != 0 || st.Run("r1").Turns != 0 {
		t.Fatalf("Check mutated state: %+v", st.Run("r1"))
	}
	e = completed("r1", 2)
	e.SessionId = "s"
	if err := st.Check(e); err == nil {
		t.Fatal("Check accepted stale attempt")
	}
}

func suspended(run string, n uint32) *v1.Event {
	return &v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: &v1.RunSuspended{RunId: run, Attempt: n}}}
}
func routed(run string, n uint32, call, node string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: run, Attempt: n, CallId: call, NodeId: node}}}
}
func approvalReq(run string, n uint32, call string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{RunId: run, Attempt: n, CallId: call}}}
}
func decided(run, call string, ok bool) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ApprovalDecided{ApprovalDecided: &v1.ApprovalDecided{RunId: run, CallId: call, Approved: ok}}}
}

func TestSuspendResumeIsNotTakeover(t *testing.T) {
	st, err := Reduce(build(
		created(), requested("r1"), attempt("r1", 1),
		assistant("r1", 1, "c1"),
		approvalReq("r1", 1, "c1"), suspended("r1", 1),
		decided("r1", "c1", true), // 外部事件，挂起期间允许
		attempt("r1", 2),          // 恢复
		routed("r1", 2, "c1", "n1"), suspended("r1", 2),
		result("r1", 0, "c1"), // 设备结果
		attempt("r1", 3),
		attempt("r1", 4), // 接管：上一个 Attempt 未挂起
	))
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Run("r1"); r.Takeovers != 1 || r.Status != RunRunning || !r.Calls[0].Done {
		t.Fatalf("run = %+v", r)
	}
}

func TestM1InvalidTransitions(t *testing.T) {
	base := []*v1.Event{created(), requested("r1"), attempt("r1", 1), assistant("r1", 1, "c1")}
	cases := map[string]struct {
		tail []*v1.Event
		want string
	}{
		"external result for local call": {[]*v1.Event{started("r1", 1, "c1"), result("r1", 0, "c1")}, "not dispatched"},
		"started before approval":        {[]*v1.Event{approvalReq("r1", 1, "c1"), started("r1", 1, "c1")}, "without approval"},
		"started after denial":           {[]*v1.Event{approvalReq("r1", 1, "c1"), decided("r1", "c1", false), started("r1", 1, "c1")}, "without approval"},
		"decide twice":                   {[]*v1.Event{approvalReq("r1", 1, "c1"), decided("r1", "c1", true), decided("r1", "c1", true)}, "not awaiting"},
		"worker event while suspended":   {[]*v1.Event{suspended("r1", 1), started("r1", 1, "c1")}, "stale attempt"},
		"re-route to another node":       {[]*v1.Event{routed("r1", 1, "c1", "n1"), attempt("r1", 2), routed("r1", 2, "c1", "n2")}, "re-routed"},
		"approval after dispatch":        {[]*v1.Event{routed("r1", 1, "c1", "n1"), approvalReq("r1", 1, "c1")}, "cannot request approval"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Reduce(build(append(append([]*v1.Event{}, base...), c.tail...)...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

func compacted(run string, n uint32, through uint64) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ContextCompacted{ContextCompacted: &v1.ContextCompacted{RunId: run, Attempt: n, ThroughSeq: through}}}
}

func TestCompactionTrimsHistory(t *testing.T) {
	st, err := Reduce(build(
		created(), requested("r1"), attempt("r1", 1), // 1-3
		assistant("r1", 1, "c1"), result("r1", 1, "c1"), // 4-5
		assistant("r1", 1, "c2"), result("r1", 1, "c2"), // 6-7
		compacted("r1", 1, 5),                           // 8
		assistant("r1", 1, "c3"), result("r1", 1, "c3"), // 9-10
		compacted("r1", 1, 9), // 11：c3 的请求在 9、结果在 10，不能在 9 截断
	))
	if err == nil || !strings.Contains(err.Error(), "splits call c3") {
		t.Fatalf("err = %v, want split error", err)
	}

	st, err = Reduce(build(
		created(), requested("r1"), attempt("r1", 1),
		assistant("r1", 1, "c1"), result("r1", 1, "c1"),
		assistant("r1", 1, "c2"), result("r1", 1, "c2"),
		compacted("r1", 1, 5),
		assistant("r1", 1, "c3"), result("r1", 1, "c3"),
		compacted("r1", 1, 10),
	))
	if err != nil {
		t.Fatal(err)
	}
	if st.Compaction.GetThroughSeq() != 10 || len(st.History) != 0 {
		t.Fatalf("compaction = %v, history = %d events", st.Compaction, len(st.History))
	}
}

func TestCompactionInvalid(t *testing.T) {
	base := []*v1.Event{created(), requested("r1"), attempt("r1", 1), assistant("r1", 1, "c1")}
	cases := map[string]struct {
		tail []*v1.Event
		want string
	}{
		"while call pending":     {[]*v1.Event{compacted("r1", 1, 4)}, "pending"},
		"splits call and result": {[]*v1.Event{result("r1", 1, "c1"), compacted("r1", 1, 4)}, "splits call c1"},
		"beyond log end":         {[]*v1.Event{result("r1", 1, "c1"), compacted("r1", 1, 6)}, "out of range"},
		"not after previous":     {[]*v1.Event{result("r1", 1, "c1"), compacted("r1", 1, 5), compacted("r1", 1, 5)}, "out of range"},
		"stale attempt":          {[]*v1.Event{result("r1", 1, "c1"), attempt("r1", 2), compacted("r1", 1, 5)}, "stale attempt"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Reduce(build(append(append([]*v1.Event{}, base...), c.tail...)...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestStalledTakeoversResetOnProgress(t *testing.T) {
	st, err := Reduce(build(
		created(), requested("r1"), attempt("r1", 1),
		attempt("r1", 2), attempt("r1", 3), // 两次无进展的接管
		assistant("r1", 3, "c1"), // 进展
		attempt("r1", 4),
		started("r1", 4, "c1"), // 开始执行不算进展
		attempt("r1", 5),
	))
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Run("r1"); r.Takeovers != 4 || r.StalledTakeovers != 2 {
		t.Fatalf("takeovers = %d, stalled = %d", r.Takeovers, r.StalledTakeovers)
	}
}

func closed() *v1.Event {
	return &v1.Event{Payload: &v1.Event_SessionClosed{SessionClosed: &v1.SessionClosed{By: "end_user:u"}}}
}

func TestSessionClosedIsTerminal(t *testing.T) {
	st, err := Reduce(build(created(), requested("r1"), attempt("r1", 1), interrupted("r1"), closed()))
	if err != nil || st.Closed == nil || st.Active() != nil {
		t.Fatalf("st = %+v, err = %v", st, err)
	}
	for name, tail := range map[string][]*v1.Event{
		"close with active run": {created(), requested("r1"), closed()},
		"input after close":     {created(), closed(), requested("r2")},
		"close twice":           {created(), closed(), closed()},
	} {
		if _, err := Reduce(build(tail...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestInputIDs：窗口内重复的输入 ID 不合法；调用结果只有外部结果（打字回答）可以带输入 ID；窗口有界。
func TestInputIDs(t *testing.T) {
	withID := func(e *v1.Event, id string) *v1.Event {
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			p.RunRequested.InputId = id
		case *v1.Event_Steered:
			p.Steered.InputId = id
		case *v1.Event_ToolResult:
			p.ToolResult.InputId = id
		}
		return e
	}
	seqd := func(events ...*v1.Event) []*v1.Event {
		for i, e := range events {
			e.Seq = uint64(i + 1)
		}
		return events
	}
	st, err := Reduce(seqd(created(), withID(requested("r1"), "a"), withID(steered("r1"), "b")))
	if err != nil {
		t.Fatal(err)
	}
	if in := st.Input("b"); in == nil || !in.Steered || in.RunID != "r1" || st.Input("a").Steered {
		t.Fatalf("inputs = %+v", st.Inputs)
	}
	if _, err := Reduce(seqd(created(), withID(requested("r1"), "a"), withID(steered("r1"), "a"))); err == nil {
		t.Fatal("duplicate input id accepted")
	}
	if _, err := Reduce(seqd(created(), requested("r1"), attempt("r1", 1), assistant("r1", 1, "c1"), withID(result("r1", 1, "c1"), "x"))); err == nil {
		t.Fatal("input id on a worker result accepted")
	}
	events := []*v1.Event{created(), requested("r1")}
	for i := range MaxRecentInputs + 5 {
		events = append(events, withID(steered("r1"), fmt.Sprintf("s%d", i)))
	}
	st, err = Reduce(seqd(events...))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Inputs) != MaxRecentInputs || st.Input("s0") != nil || st.Input(fmt.Sprintf("s%d", MaxRecentInputs+4)) == nil {
		t.Fatalf("window holds %d inputs", len(st.Inputs))
	}
}

func callStarted(id string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_CallStarted{CallStarted: &v1.CallStarted{CallId: id, DeviceId: "phone"}}}
}
func transcript(id, role, text string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_CallTranscript{CallTranscript: &v1.CallTranscript{CallId: id, Role: role, Text: text}}}
}
func callEnded(id string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_CallEnded{CallEnded: &v1.CallEnded{CallId: id, Reason: "hangup"}}}
}
func fromCall(e *v1.Event, id string) *v1.Event {
	switch p := e.GetPayload().(type) {
	case *v1.Event_RunRequested:
		p.RunRequested.FromCall = id
	case *v1.Event_Steered:
		p.Steered.FromCall = id
	}
	return e
}

// TestCalls：一个 Session 同一时刻至多一个 Call；转写与派生的输入只属于进行中的 Call；转写进入对话上下文；
// 关闭 Session 前须结束 Call。
func TestCalls(t *testing.T) {
	st, err := Reduce(build(created(), callStarted("a"), transcript("a", "user", "hi"), fromCall(requested("r1"), "a"),
		fromCall(steered("r1"), "a"), transcript("a", "assistant", "ok"), callEnded("a"), callStarted("b")))
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveCall == nil || st.ActiveCall.ID != "b" || st.ActiveCall.DeviceID != "phone" {
		t.Fatalf("active call = %+v", st.ActiveCall)
	}
	n := 0
	for _, e := range st.History {
		if e.GetCallTranscript() != nil {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("history holds %d transcripts", n)
	}
	for name, events := range map[string][]*v1.Event{
		"two calls at once":           {created(), callStarted("a"), callStarted("b")},
		"transcript without a call":   {created(), transcript("a", "user", "hi")},
		"transcript of another call":  {created(), callStarted("a"), transcript("b", "user", "hi")},
		"transcript with a bad role":  {created(), callStarted("a"), transcript("a", "system", "hi")},
		"empty transcript":            {created(), callStarted("a"), transcript("a", "user", "")},
		"run from an ended call":      {created(), callStarted("a"), callEnded("a"), fromCall(requested("r1"), "a")},
		"steer from an inactive call": {created(), requested("r1"), fromCall(steered("r1"), "a")},
		"end an unknown call":         {created(), callEnded("a")},
		"close during a call":         {created(), callStarted("a"), closed()},
		"call without an id":          {created(), callStarted("")},
	} {
		if _, err := Reduce(build(events...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// 工作类别按 Run 已做的工作判定（ADR-0029）：回合与压缩都计入；用户做出审批后的长任务在后台池中先被认领，
// 名额由下一个 Attempt 消费。
func TestWorkClass(t *testing.T) {
	events := []*v1.Event{created(), requested("r1"), attempt("r1", 1)}
	for i := range workqueue.LongRunTurns - 2 {
		c := fmt.Sprintf("c%d", i)
		events = append(events, assistant("r1", 1, c), result("r1", 1, c))
	}
	st, err := Reduce(build(events...))
	if err != nil {
		t.Fatal(err)
	}
	if p := st.WorkClass().Priority; p != workqueue.Interactive {
		t.Fatalf("after %d turns: priority %d", workqueue.LongRunTurns-2, p)
	}
	events = append(events, compacted("r1", 1, st.Seq), assistant("r1", 1, "ca"), approvalReq("r1", 1, "ca"), suspended("r1", 1))
	if st, err = Reduce(build(events...)); err != nil {
		t.Fatal(err)
	}
	if p := st.WorkClass().Priority; p != workqueue.Background {
		t.Fatalf("turns plus a compaction reach the threshold: priority %d", p)
	}
	events = append(events, decided("r1", "ca", true))
	if st, err = Reduce(build(events...)); err != nil {
		t.Fatal(err)
	}
	if p := st.WorkClass().Priority; p != workqueue.Resumed {
		t.Fatalf("after the user's decision: priority %d", p)
	}
	events = append(events, attempt("r1", 2))
	if st, err = Reduce(build(events...)); err != nil {
		t.Fatal(err)
	}
	if p := st.WorkClass().Priority; p != workqueue.Background {
		t.Fatalf("resume slot not consumed by the next attempt: priority %d", p)
	}

	// 回合不多、输出很长的 Run 同样是长任务。
	long := assistant("r2", 1)
	long.GetAssistantMessage().Usage = &v1.Usage{OutputTokens: workqueue.LongRunOutputTokens}
	if st, err = Reduce(build(created(), requested("r2"), attempt("r2", 1), long)); err != nil {
		t.Fatal(err)
	}
	if p := st.WorkClass().Priority; p != workqueue.Background {
		t.Fatalf("long output: priority %d", p)
	}
}

// 投影有界（延展性评审 §8.3）：旧 Run 只剩摘要，克隆只复制最近的调用 ID；运行 ID 与调用 ID 的唯一性仍对全部历史检查。
func TestBoundedProjection(t *testing.T) {
	events := []*v1.Event{created()}
	const runs = 40
	for i := range runs {
		run := fmt.Sprintf("r%d", i)
		events = append(events, requested(run), attempt(run, 1))
		for j := range 10 {
			c := fmt.Sprintf("c%d_%d", i, j)
			events = append(events, assistant(run, 1, c), result(run, 1, c))
		}
		events = append(events, assistant(run, 1), completed(run, 1))
	}
	st, err := Reduce(build(events...))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Runs) > KeepRuns+1 || len(st.Summaries()) != runs || st.Past[0].ID != "r0" || st.Past[0].Status != RunCompleted || st.Past[0].Turns != 11 {
		t.Fatalf("runs %d, summaries %d, first %+v", len(st.Runs), len(st.Summaries()), st.Past[0])
	}
	if cp := st.Clone(); len(cp.callIDs) >= callIDBatch {
		t.Fatalf("clone copied %d call ids", len(cp.callIDs))
	}
	seq := uint64(len(events))
	reuse := func(e *v1.Event) error { e.SessionId, e.Seq = "s", seq+1; return st.Check(e) }
	if err := reuse(requested("r0")); err == nil || !strings.Contains(err.Error(), "duplicate run id") {
		t.Fatalf("reused run id of a trimmed run: %v", err)
	}
	next := build(append(append([]*v1.Event{}, events...), requested("rx"), attempt("rx", 1))...)
	if st, err = Reduce(next); err != nil {
		t.Fatal(err)
	}
	seq = uint64(len(next))
	if err := reuse(assistant("rx", 1, "c0_0")); err == nil || !strings.Contains(err.Error(), "duplicate call id") {
		t.Fatalf("reused call id of a trimmed run: %v", err)
	}
}

// 历史扩大十倍时，每次提交的校验（克隆加应用）代价不随之线性增长（延展性评审 §8.4 第 2 项）。
func TestCheckCostIndependentOfHistory(t *testing.T) {
	stateWith := func(runs int) *State {
		events := []*v1.Event{created()}
		for i := range runs {
			run := fmt.Sprintf("r%d", i)
			events = append(events, requested(run), attempt(run, 1))
			for j := range 5 {
				c := fmt.Sprintf("c%d_%d", i, j)
				events = append(events, assistant(run, 1, c), result(run, 1, c))
			}
			// 上下文压缩使 History 随窗口有界（ADR-0012），否则它本身随历史增长。
			events = append(events, compacted(run, 1, uint64(len(events))), assistant(run, 1), completed(run, 1))
		}
		events = append(events, requested("live"), attempt("live", 1))
		st, err := Reduce(build(events...))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	// 每次校验分配的字节数：克隆复制的是指针与 ID，次数不随历史变化，字节数会。
	bytesPerCheck := func(st *State) uint64 {
		e := steered("live")
		e.SessionId, e.Seq = "s", st.Seq+1
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range 50 {
			if err := st.Check(e); err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / 50
	}
	small, large := bytesPerCheck(stateWith(100)), bytesPerCheck(stateWith(1000))
	if large > 2*small+4096 {
		t.Fatalf("check allocates %d bytes with 1000 past runs vs %d with 100", large, small)
	}
}

// 委托 Run 记在投影的固定位置，不随去重窗口淘汰；快照恢复后不变（评审 §11 问题 3）。
func TestDelegatedRunOutlivesInputWindow(t *testing.T) {
	child := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u",
		Parent: &v1.ParentRef{SessionId: "p", RunId: "pr", CallId: "pc"}}}}
	task := &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r0", InputId: DelegatedInputID("pc")}}}
	events := []*v1.Event{child, task, interrupted("r0")}
	for i := range MaxRecentInputs + 8 {
		run := fmt.Sprintf("u%d", i)
		events = append(events, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: run, InputId: "in_" + run}}},
			interrupted(run))
	}
	st, err := Reduce(build(events...))
	if err != nil {
		t.Fatal(err)
	}
	if st.Input(DelegatedInputID("pc")) != nil {
		t.Fatal("test setup: the delegated input is still in the window")
	}
	if st.Delegated != "r0" {
		t.Fatalf("delegated run %q, want r0", st.Delegated)
	}
	restored, err := FromSnapshot(st.Snapshot())
	if err != nil || restored.Delegated != "r0" {
		t.Fatalf("delegated run after snapshot %q, %v", restored.Delegated, err)
	}
	// 不是子 Session 时，同名的输入 ID 不算委托任务。
	plain, err := Reduce(build(created(), &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{
		RunId: "r0", InputId: DelegatedInputID("pc")}}}))
	if err != nil || plain.Delegated != "" {
		t.Fatalf("top-level session recorded a delegated run %q, %v", plain.Delegated, err)
	}
}
