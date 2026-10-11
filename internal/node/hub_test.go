package node_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

// downLog 在 down 时拒绝一切追加（数据库不可用）。
type downLog struct {
	*memlog.Log
	down bool
}

func (l *downLog) Append(ctx context.Context, sid string, expected uint64, events ...*v1.Event) (uint64, error) {
	if l.down {
		return 0, errors.New("database unavailable")
	}
	return l.Log.Append(ctx, sid, expected, events...)
}

// 结果没有写入日志时不能确认：Inbox 保留调用，设备重连后从账本重新返回，结果最终写入。
// 已经作废的结果（调用已有结果）则确认并丢弃。
func TestResultNotAcknowledgedUntilStored(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	log := &downLog{Log: memlog.New()}
	store := &session.Store{Log: log, IDs: ids.Sequential("e"), Clock: clk}
	h := &node.Hub{Dir: node.NewMemDirectory(clk), Inbox: node.NewMemInbox(), Store: store, Queue: memqueue.New(clk), Clock: clk}

	st := &session.State{SessionID: "s1"}
	if err := store.Commit(ctx, st,
		&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "a", Version: "1"}}}},
		&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("hi")}}},
		&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: "r1", Attempt: 1}}},
		&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: "r1", Attempt: 1,
			ToolCalls: []*v1.ToolCall{{CallId: "c1", Capability: "pc__read_file", ArgumentsJson: "{}"}}}}},
		&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: "r1", Attempt: 1, CallId: "c1", NodeId: "n1"}}},
	); err != nil {
		t.Fatal(err)
	}
	inv := &v1.Invoke{SessionId: "s1", RunId: "r1", CallId: "c1", Capability: "read_file"}
	if err := h.Inbox.Put(ctx, "n1", inv); err != nil {
		t.Fatal(err)
	}
	res := &v1.InvokeResult{CallId: "c1", Content: model.TextBlocks("内容")}

	log.down = true
	if err := h.Result(ctx, "n1", res); err == nil {
		t.Fatal("result reported as accepted while the log was down")
	}
	if items, _, _ := h.Inbox.Pending(ctx, "n1"); len(items) != 1 {
		t.Fatal("call withdrawn from the inbox although its result was not stored")
	}

	log.down = false
	if err := h.Result(ctx, "n1", res); err != nil {
		t.Fatal(err)
	}
	final, _ := store.Load(ctx, "s1")
	if c := final.Run("r1").Calls[0]; !c.Done {
		t.Fatal("result not stored after redelivery")
	}
	// 重复的结果（调用已有结果）作废：确认并撤下。
	if err := h.Inbox.Put(ctx, "n1", inv); err != nil {
		t.Fatal(err)
	}
	if err := h.Result(ctx, "n1", res); err != nil {
		t.Fatalf("duplicate result: %v", err)
	}
	if items, _, _ := h.Inbox.Pending(ctx, "n1"); len(items) != 0 {
		t.Fatal("duplicate result left the call in the inbox")
	}
}
