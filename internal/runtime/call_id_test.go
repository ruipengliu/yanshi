package runtime

import (
	"context"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

type repeatedCallIDs struct{}

func (repeatedCallIDs) Generate(context.Context, *model.Request, func(model.Delta)) (*model.Response, error) {
	return &model.Response{ToolCalls: []*v1.ToolCall{
		{CallId: "call_0", Capability: "note", ArgumentsJson: `{}`},
		{Capability: "note", ArgumentsJson: `{}`},
	}}, nil
}

// 模型给的 ID 可能跨轮重复或为空，一律改写为继承 Session 槽位的平台 ID。
func TestWorkerRewritesCallIDs(t *testing.T) {
	ctx := context.Background()
	newID, _ := ids.SessionID("parent", "u")
	for _, sid := range []string{newID} {
		t.Run(sid, func(t *testing.T) {
			gen := ids.Sequential("id")
			consumed := 0
			store := &session.Store{Log: memlog.New(), Clock: clock.Real{}, IDs: func() string { consumed++; return gen() }}
			q := memqueue.New(clock.Real{})
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "m/any", Capabilities: []string{"note"}})
			if err != nil {
				t.Fatal(err)
			}
			st := &session.State{SessionID: sid}
			if err := store.Commit(ctx, st,
				&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "a", Version: "1"}}}},
				&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r", Input: model.TextBlocks("work")}}}); err != nil {
				t.Fatal(err)
			}
			if err := q.Enqueue(ctx, sid, workqueue.Class{}); err != nil {
				t.Fatal(err)
			}
			gw := model.NewGateway()
			gw.Register("m", repeatedCallIDs{})
			w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw,
				Catalog: &capability.Catalog{Local: capability.NewRegistry(namedCap("note"))}})
			for range 8 {
				before := consumed
				if _, err := w.Step(ctx); err != nil {
					t.Fatal(err)
				}
				events, err := eventlog.ReadAll(ctx, store.Log, sid, 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range events {
					m := e.GetAssistantMessage()
					if m == nil {
						continue
					}
					calls := m.GetToolCalls()
					if len(calls) != 2 || calls[0].GetCallId() == "call_0" || calls[0].GetCallId() == calls[1].GetCallId() {
						t.Fatalf("model IDs not rewritten: %v", calls)
					}
					// 模型计量、两个调用、一个 AssistantMessage。
					if consumed-before != 4 {
						t.Fatalf("model step consumed %d IDs, want 4", consumed-before)
					}
					wantSlot, _ := ids.SessionSlotOf(sid)
					for _, c := range calls {
						slot, err := ids.SlotOf(c.GetCallId())
						if err != nil || slot != wantSlot || !strings.HasPrefix(c.GetCallId(), "call_") {
							t.Fatalf("call ID %s: %v", c.GetCallId(), err)
						}
					}
					return
				}
			}
			t.Fatal("no assistant message")
		})
	}
}
