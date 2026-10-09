package session

import (
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
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
