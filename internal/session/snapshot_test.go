package session_test

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/session"
	"yanshi/internal/session/snapshottest"
)

func TestMemSnapshots(t *testing.T) {
	snapshottest.Run(t, func(*testing.T) session.Snapshots { return session.NewMemSnapshots() })
}

// richLog 覆盖投影的全部字段：多个 Run、审批、路由调用、召回、压缩、关闭。
func richLog() []*v1.Event {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := func(p any) *v1.Event {
		e := &v1.Event{Time: timestamppb.New(t0)}
		switch p := p.(type) {
		case *v1.SessionCreated:
			e.Payload = &v1.Event_SessionCreated{SessionCreated: p}
		case *v1.RunRequested:
			e.Payload = &v1.Event_RunRequested{RunRequested: p}
		case *v1.AttemptStarted:
			e.Payload = &v1.Event_AttemptStarted{AttemptStarted: p}
		case *v1.MemoryRecalled:
			e.Payload = &v1.Event_MemoryRecalled{MemoryRecalled: p}
		case *v1.AssistantMessage:
			e.Payload = &v1.Event_AssistantMessage{AssistantMessage: p}
		case *v1.ApprovalRequested:
			e.Payload = &v1.Event_ApprovalRequested{ApprovalRequested: p}
		case *v1.RunSuspended:
			e.Payload = &v1.Event_RunSuspended{RunSuspended: p}
		case *v1.ApprovalDecided:
			e.Payload = &v1.Event_ApprovalDecided{ApprovalDecided: p}
		case *v1.ToolCallStarted:
			e.Payload = &v1.Event_ToolCallStarted{ToolCallStarted: p}
		case *v1.ToolResult:
			e.Payload = &v1.Event_ToolResult{ToolResult: p}
		case *v1.ContextCompacted:
			e.Payload = &v1.Event_ContextCompacted{ContextCompacted: p}
		case *v1.RunCompleted:
			e.Payload = &v1.Event_RunCompleted{RunCompleted: p}
		case *v1.Steered:
			e.Payload = &v1.Event_Steered{Steered: p}
		case *v1.RunInterrupted:
			e.Payload = &v1.Event_RunInterrupted{RunInterrupted: p}
		case *v1.SessionClosed:
			e.Payload = &v1.Event_SessionClosed{SessionClosed: p}
		}
		return e
	}
	text := []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "x"}}}}
	call := func(id string) []*v1.ToolCall {
		return []*v1.ToolCall{{CallId: id, Capability: "pc__send", ArgumentsJson: "{}"}}
	}
	return []*v1.Event{
		ev(&v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "a", Version: "1"}}),
		ev(&v1.RunRequested{RunId: "r1", Input: text}),
		ev(&v1.AttemptStarted{RunId: "r1", Attempt: 1, LiveEndpoint: "http://p1"}),
		ev(&v1.MemoryRecalled{RunId: "r1", Attempt: 1, Items: []*v1.RecalledMemory{{Id: "m1", BusinessLine: "bl", Category: "plan", Content: "c"}}}),
		ev(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: call("c1")}),
		ev(&v1.ApprovalRequested{RunId: "r1", Attempt: 1, CallId: "c1", Summary: "s", Deadline: timestamppb.New(t0.Add(time.Hour))}),
		ev(&v1.RunSuspended{RunId: "r1", Attempt: 1}),
		ev(&v1.ApprovalDecided{RunId: "r1", CallId: "c1", Approved: true}),
		ev(&v1.AttemptStarted{RunId: "r1", Attempt: 2, LiveEndpoint: "http://p2"}),
		ev(&v1.ToolCallStarted{RunId: "r1", Attempt: 2, CallId: "c1", NodeId: "n1", Deadline: timestamppb.New(t0.Add(time.Minute))}),
		ev(&v1.RunSuspended{RunId: "r1", Attempt: 2}),
		ev(&v1.ToolResult{RunId: "r1", CallId: "c1", Content: text}),
		ev(&v1.AttemptStarted{RunId: "r1", Attempt: 3}),
		ev(&v1.AttemptStarted{RunId: "r1", Attempt: 4}), // 接管
		ev(&v1.ContextCompacted{RunId: "r1", Attempt: 4, ThroughSeq: 12, Summary: text}),
		ev(&v1.AssistantMessage{RunId: "r1", Attempt: 4, Content: text}),
		ev(&v1.RunCompleted{RunId: "r1", Attempt: 4}),
		ev(&v1.RunRequested{RunId: "r2", Input: text}),
		ev(&v1.Steered{RunId: "r2", Input: text}),
		ev(&v1.RunInterrupted{RunId: "r2", By: "user"}),
		ev(&v1.SessionClosed{By: "user"}),
	}
}

// TestSnapshotAtEveryPositionMatchesReplay：在日志的每个位置取快照，恢复后应用剩余事件，
// 结果必须与完整回放一致；快照每 1 条写一次时，Load 的结果也必须一致。
func TestSnapshotAtEveryPositionMatchesReplay(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	events := richLog()
	if _, err := log.Append(ctx, "s", 0, events...); err != nil {
		t.Fatal(err)
	}
	all, _ := log.Read(ctx, "s", 0, 0)
	full, err := session.Reduce(all)
	if err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= len(all); k++ {
		prefix, err := session.Reduce(all[:k])
		if err != nil {
			t.Fatal(err)
		}
		st, err := session.FromSnapshot(prefix.Snapshot())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range all[k:] {
			if err := st.Apply(e); err != nil {
				t.Fatalf("snapshot at %d: apply seq %d: %v", k, e.GetSeq(), err)
			}
		}
		if !session.Equal(st, full) {
			t.Fatalf("snapshot at %d diverges from replay", k)
		}
	}
	store := &session.Store{Log: log, IDs: ids.Sequential("e"), Clock: clock.Real{}, Snapshots: session.NewMemSnapshots(), SnapshotEvery: 1}
	for range 2 { // 第一次写快照，第二次从快照加载
		st, err := store.Load(ctx, "s")
		if err != nil || !session.Equal(st, full) {
			t.Fatalf("load = %v", err)
		}
	}
	if _, err := session.FromSnapshot(&v1.SessionSnapshot{Version: session.ProjectionVersion + 1}); err == nil {
		t.Fatal("accepted a snapshot of another projection version")
	}
}

// TestSnapshotIgnoredAfterLogDeleted：日志删除后残留的快照不能让 Session 复活。
func TestSnapshotIgnoredAfterLogDeleted(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	_, _ = log.Append(ctx, "s", 0, richLog()...)
	snaps := session.NewMemSnapshots()
	store := &session.Store{Log: log, IDs: ids.Sequential("e"), Clock: clock.Real{}, Snapshots: snaps, SnapshotEvery: 1}
	if _, err := store.Load(ctx, "s"); err != nil || !snaps.Has("s") {
		t.Fatalf("snapshot not written: %v", err)
	}
	_ = log.Delete(ctx, "s")
	st, err := store.Load(ctx, "s")
	if err != nil || st.Created != nil || st.Seq != 0 {
		t.Fatalf("deleted session resurrected from snapshot: seq %d, %v", st.Seq, err)
	}
}

// TestSnapshotCoversEveryField：投影新增字段时，必须同时写进快照（Snapshot/FromSnapshot）并加入这里的清单，
// 否则从快照恢复会悄悄丢失它——而 Equal 按快照比较，看不出差别。
func TestSnapshotCoversEveryField(t *testing.T) {
	covered := map[string][]string{
		"State":    {"SessionID", "Seq", "Created", "Closed", "Compaction", "CompactedAt", "Runs", "History", "callIDs"},
		"Run":      {"ID", "Status", "RequestedAt", "Attempt", "LiveEndpoint", "Takeovers", "StalledTakeovers", "Turns", "Recall", "Recalled", "Calls"},
		"Call":     {"Call", "StartedAttempts", "NodeID", "Deadline", "Done", "Approval"},
		"Approval": {"Summary", "Deadline", "Decided", "Approved"},
	}
	for _, v := range []any{session.State{}, session.Run{}, session.Call{}, session.Approval{}} {
		typ := reflect.TypeOf(v)
		want := covered[typ.Name()]
		var got []string
		for i := range typ.NumField() {
			got = append(got, typ.Field(i).Name)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s fields %v, snapshot covers %v: update snapshot.go and bump ProjectionVersion", typ.Name(), got, want)
		}
	}
}
