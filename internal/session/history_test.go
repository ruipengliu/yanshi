package session_test

import (
	"context"
	"fmt"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/session"
)

// Run 移出投影之后，调用的记录从日志中查到：重复的审批与回答据此返回 conflict，而不是 not found。
func TestHistoryCallScansTrimmedRuns(t *testing.T) {
	ctx := context.Background()
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("e"), Clock: clock.Real{}}
	st := &session.State{SessionID: "s"}
	commit := func(events ...*v1.Event) {
		t.Helper()
		if err := store.Commit(ctx, st, events...); err != nil {
			t.Fatal(err)
		}
	}
	commit(&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}})
	for i := range session.KeepRuns + 3 {
		run := fmt.Sprintf("r%d", i)
		ask, risky := fmt.Sprintf("ask%d", i), fmt.Sprintf("risky%d", i)
		commit(&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: run}}},
			&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: run, Attempt: 1}}},
			&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: run, Attempt: 1, ToolCalls: []*v1.ToolCall{
				{CallId: ask, Capability: "ask_user"}, {CallId: risky, Capability: "pc__send"}}}}},
			&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: run, Attempt: 1, CallId: ask}}},
			&v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{RunId: run, Attempt: 1, CallId: risky}}},
			&v1.Event{Payload: &v1.Event_RunInterrupted{RunInterrupted: &v1.RunInterrupted{RunId: run}}})
	}
	if len(st.Past) == 0 {
		t.Fatal("no run was trimmed")
	}
	for _, c := range []struct {
		call string
		want session.CallHistory
	}{
		{"ask0", session.CallHistory{Asked: true}}, // 已移出投影
		{"risky0", session.CallHistory{ApprovalRequested: true}},
		{fmt.Sprintf("ask%d", session.KeepRuns+2), session.CallHistory{Asked: true}}, // 仍在投影中
		{"nope", session.CallHistory{}},
	} {
		got, err := store.HistoryCall(ctx, st, c.call, "ask_user")
		if err != nil || got != c.want {
			t.Fatalf("%s: %+v, %v; want %+v", c.call, got, err, c.want)
		}
	}
}
