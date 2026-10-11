package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// sendOnce 的模型先请求一次 send，拿到结果后结束。
type sendOnce struct{}

func (sendOnce) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	for _, m := range req.Messages {
		if m.Role == model.RoleTool {
			return &model.Response{Content: model.TextBlocks("已发送")}, nil
		}
	}
	return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "c1", Capability: "send", ArgumentsJson: "{}"}}}, nil
}

// failingResults 让 ToolResult 的追加失败 n 次（存储故障，不是冲突）。
type failingResults struct {
	*memlog.Log
	n int
}

func (l *failingResults) Append(ctx context.Context, sid string, expected uint64, events ...*v1.Event) (uint64, error) {
	for _, e := range events {
		if e.GetToolResult() != nil && l.n > 0 {
			l.n--
			return 0, errors.New("simulated storage failure")
		}
	}
	return l.Log.Append(ctx, sid, expected, events...)
}

// 结果写入失败后，同一 Attempt 只重试写入，不再执行非幂等的调用。
func TestResultCommitFailureDoesNotReexecute(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	q := memqueue.New(clk)
	log := &failingResults{Log: memlog.New(), n: 2}
	store := &session.Store{Log: log, IDs: ids.Sequential("id"), Clock: clk}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any", Capabilities: []string{"send"}})
	gw := model.NewGateway()
	gw.Register("v", sendOnce{})
	sends := 0
	send := capability.Func{
		S: capability.Spec{Name: "send", Idempotent: false, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Fn: func(context.Context, capability.Invocation) ([]*v1.ContentBlock, error) {
			sends++
			return model.TextBlocks("sent"), nil
		},
	}
	w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw,
		Catalog: &capability.Catalog{Local: capability.NewRegistry(send)}})

	st := &session.State{SessionID: "s1"}
	created := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "v", Version: "1"}}}}
	if err := store.Commit(ctx, st, created, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("发吧")}}}); err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(ctx, "s1", workqueue.Class{})
	for range 12 {
		_, _ = w.Step(ctx)
	}
	final, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if r := final.Run("r1"); r.Status != session.RunCompleted {
		t.Fatalf("run %s, want completed", r.Status)
	}
	if sends != 1 {
		t.Fatalf("send executed %d times, want 1", sends)
	}
}
