package session

import (
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

func readOnlyStarted(run string, n uint32, call string, readOnly bool) *v1.Event {
	e := started(run, n, call)
	e.GetToolCallStarted().ReadOnly = readOnly
	return e
}

func TestCallsReadOnly(t *testing.T) {
	failedWithoutStart := result("r", 1, "a")
	failedWithoutStart.GetToolResult().IsError = true
	for _, tc := range []struct {
		name string
		tail []*v1.Event
		want bool
	}{
		{"no calls", nil, true},
		{"all read only", []*v1.Event{
			assistant("r", 1, "a", "b"), readOnlyStarted("r", 1, "a", true), result("r", 1, "a"),
			readOnlyStarted("r", 1, "b", true), result("r", 1, "b"),
		}, true},
		{"one write", []*v1.Event{
			assistant("r", 1, "a", "b"), readOnlyStarted("r", 1, "a", true), result("r", 1, "a"),
			readOnlyStarted("r", 1, "b", false), result("r", 1, "b"),
		}, false},
		{"failed without start", []*v1.Event{assistant("r", 1, "a"), failedWithoutStart}, false},
		{"old log", []*v1.Event{assistant("r", 1, "a"), started("r", 1, "a"), result("r", 1, "a")}, false},
		{"takeover all read only", []*v1.Event{
			assistant("r", 1, "a"), readOnlyStarted("r", 1, "a", true), attempt("r", 2),
			readOnlyStarted("r", 2, "a", true), result("r", 2, "a"),
		}, true},
		{"takeover from write", []*v1.Event{
			assistant("r", 1, "a"), readOnlyStarted("r", 1, "a", false), attempt("r", 2),
			readOnlyStarted("r", 2, "a", true), result("r", 2, "a"),
		}, false},
		{"takeover to write", []*v1.Event{
			assistant("r", 1, "a"), readOnlyStarted("r", 1, "a", true), attempt("r", 2),
			readOnlyStarted("r", 2, "a", false), result("r", 2, "a"),
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]*v1.Event{created(), requested("r"), attempt("r", 1)}, tc.tail...)
			st, err := Reduce(build(events...))
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Run("r").CallsReadOnly(); got != tc.want {
				t.Fatalf("CallsReadOnly = %v, want %v", got, tc.want)
			}
			// 真正经过序列化，防止快照漏字段或把 false 当成缺失。
			data, err := proto.Marshal(st.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			snap := &v1.SessionSnapshot{}
			if err := proto.Unmarshal(data, snap); err != nil {
				t.Fatal(err)
			}
			restored, err := FromSnapshot(snap)
			if err != nil {
				t.Fatal(err)
			}
			if !Equal(st, restored) || restored.Run("r").CallsReadOnly() != tc.want {
				t.Fatal("snapshot lost read-only evidence")
			}
		})
	}
	var missing *Run
	if missing.CallsReadOnly() {
		t.Fatal("missing run has evidence")
	}
}

func TestReadOnlyEvidenceSurvivesCloneAndRejectsDuplicate(t *testing.T) {
	st, err := Reduce(build(created(), requested("r"), attempt("r", 1), assistant("r", 1, "a"),
		readOnlyStarted("r", 1, "a", true), attempt("r", 2), readOnlyStarted("r", 2, "a", false)))
	if err != nil {
		t.Fatal(err)
	}
	c := st.Run("r").Calls[0]
	if !c.ReadOnlyInAttempt(1) || c.ReadOnlyInAttempt(2) || c.ReadOnlyInAttempt(3) || st.Run("r").CallsReadOnly() {
		t.Fatal("evidence was overwritten across attempts")
	}
	cp := st.Clone()
	cp.Run("r").Calls[0].StartedReadOnly[0] = false
	if !c.ReadOnlyInAttempt(1) {
		t.Fatal("clone shares mutable evidence")
	}
	duplicate := readOnlyStarted("r", 2, "a", true)
	duplicate.SessionId, duplicate.Seq = st.SessionID, st.Seq+1
	if err := st.Apply(duplicate); err == nil {
		t.Fatal("accepted duplicate Started with changed evidence")
	}
	if c.ReadOnlyInAttempt(2) || st.Run("r").CallsReadOnly() {
		t.Fatal("rejected event changed evidence")
	}
}
