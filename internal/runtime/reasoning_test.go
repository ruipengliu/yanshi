package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// thinking 先输出思考过程再回复，并记下每次请求，供检查思考过程没有被发回给模型。
type thinking struct {
	clk      *clock.Fake
	reason   string
	requests []*model.Request
}

func (m *thinking) Generate(_ context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	m.requests = append(m.requests, req)
	m.clk.Advance(300 * time.Millisecond)
	onDelta(model.Delta{Reasoning: m.reason})
	m.clk.Advance(time.Second)
	onDelta(model.Delta{Text: "七点"})
	return &model.Response{Content: model.TextBlocks("七点"), Reasoning: m.reason, FinishReason: "stop",
		Usage: &v1.Usage{OutputTokens: 9, ReasoningTokens: 6}}, nil
}

type recordingBus struct{ deltas []live.Delta }

func (b *recordingBus) Publish(d live.Delta) { b.deltas = append(b.deltas, d) }
func (b *recordingBus) Subscribe(string) (<-chan live.Delta, func()) {
	return make(chan live.Delta), func() {}
}

// TestReasoningRecorded：思考过程以增量推送、随回复写入日志，带上耗时、首字延迟与结束原因；之后的请求不含它
// （ADR-0034）。思考过程中的违规内容与回复一样被拦截，不写入日志。
func TestReasoningRecorded(t *testing.T) {
	ctx := context.Background()
	run := func(reason string) ([]*v1.Event, *thinking, *recordingBus) {
		clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
		q := memqueue.New(clk)
		store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
		agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "t", Version: "1", Model: "t/any"})
		m := &thinking{clk: clk, reason: reason}
		gw := model.NewGateway()
		gw.Register("t", m)
		bus := &recordingBus{}
		w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw, Moderator: moderation.Mock{}, Live: bus,
			Catalog: &capability.Catalog{Local: capability.NewRegistry()}})
		st := &session.State{SessionID: "s1"}
		created := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "t", Version: "1"}}}}
		for _, in := range []string{"几点", "再问一次"} {
			events := []*v1.Event{{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r_" + in, Input: model.TextBlocks(in)}}}}
			if created != nil {
				events, created = append([]*v1.Event{created}, events...), nil
			}
			if err := store.Commit(ctx, st, events...); err != nil {
				t.Fatal(err)
			}
			_ = q.Enqueue(ctx, "s1", workqueue.Class{})
			for range 6 {
				if _, err := w.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			_ = store.Sync(ctx, st)
		}
		events, _ := eventlog.ReadAll(ctx, store.Log, "s1", 0)
		return events, m, bus
	}

	events, m, bus := run("用户在问时间")
	var msgs []*v1.AssistantMessage
	for _, e := range events {
		if a := e.GetAssistantMessage(); a != nil {
			msgs = append(msgs, a)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("assistant messages = %d", len(msgs))
	}
	a := msgs[0]
	if a.GetReasoning() != "用户在问时间" || a.GetFinishReason() != "stop" || a.GetDurationMs() != 1300 || a.GetFirstTokenMs() != 300 || a.GetUsage().GetReasoningTokens() != 6 {
		t.Fatalf("assistant message %v", a)
	}
	streamed := false
	for _, d := range bus.deltas {
		streamed = streamed || d.Reasoning == "用户在问时间"
	}
	if !streamed {
		t.Fatalf("reasoning was not streamed: %+v", bus.deltas)
	}
	for _, msg := range m.requests[1].Messages {
		if strings.Contains(model.Text(msg.Content), "用户在问时间") {
			t.Fatal("reasoning was sent back to the model")
		}
	}

	events, _, _ = run("思考中出现【违规测试】")
	for _, e := range events {
		if strings.Contains(e.GetAssistantMessage().GetReasoning(), "违规") || strings.Contains(model.Text(e.GetAssistantMessage().GetContent()), "七点") {
			t.Fatalf("blocked reasoning was committed: %v", e)
		}
	}
}
