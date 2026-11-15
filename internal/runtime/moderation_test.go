package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// violating 的回复包含违规标记，并请求一次调用（以用户名义发送）。
type violating struct{}

func (violating) Generate(context.Context, *model.Request, func(model.Delta)) (*model.Response, error) {
	return &model.Response{Content: model.TextBlocks("好的【违规测试】"),
		ToolCalls: []*v1.ToolCall{{CallId: "c", Capability: "clock_now", ArgumentsJson: "{}"}}}, nil
}

type toggling struct{ fail bool }

func (t *toggling) Check(ctx context.Context, req moderation.Request) (moderation.Verdict, error) {
	if t.fail {
		return moderation.Verdict{}, errors.New("timeout")
	}
	return moderation.Mock{}.Check(ctx, req)
}

// TestOutputModeration：服务不可用时不写入任何输出，Run 挂起等待（ADR-0054）；恢复后违规回复被替换为拒答文本、
// 调用被丢弃，日志中有 ContentModerated 且没有违规原文。
func TestOutputModeration(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	q := memqueue.New(clk)
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any", Capabilities: []string{"clock_now"}})
	gw := model.NewGateway()
	gw.Register("v", violating{})
	mod := &toggling{fail: true}
	w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw, Moderator: mod,
		Catalog: &capability.Catalog{Local: capability.NewRegistry(capability.ClockNow(clk))}})

	st := &session.State{SessionID: "s1"}
	created := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "v", Version: "1"}}}}
	if err := store.Commit(ctx, st, created, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("hi")}}}); err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(ctx, "s1", workqueue.Class{})
	for range 4 {
		_, _ = w.Step(ctx)
	}
	events, _ := eventlog.ReadAll(ctx, store.Log, "s1", 0)
	var suspended bool
	for _, e := range events {
		if e.GetAssistantMessage() != nil {
			t.Fatal("output committed while moderation was unavailable")
		}
		if s := e.GetRunSuspended(); s != nil && s.GetReason() == session.SuspendModeration {
			suspended = true
		}
	}
	if !suspended {
		t.Fatal("run did not suspend while moderation was unavailable")
	}

	mod.fail = false
	clk.Advance(30 * time.Second) // 第一次退避到期
	for range 4 {
		if _, err := w.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	final, _ := store.Load(ctx, "s1")
	if r := final.Run("r1"); r.Status != session.RunCompleted || len(r.Calls) != 0 {
		t.Fatalf("run %s with %d calls, want completed without calls", r.Status, len(r.Calls))
	}
	events, _ = eventlog.ReadAll(ctx, store.Log, "s1", 0)
	var moderated bool
	for _, e := range events {
		if m := e.GetContentModerated(); m != nil {
			moderated = m.GetStage() == "output" && m.GetLabels()[0] == "mock"
		}
		if a := e.GetAssistantMessage(); a != nil && model.Text(a.GetContent()) != moderation.Refusal {
			t.Fatalf("assistant message %q, want the refusal", model.Text(a.GetContent()))
		}
		if j, _ := protojson.Marshal(e); strings.Contains(string(j), "违规测试") {
			t.Fatal("blocked output reached the log")
		}
	}
	if !moderated {
		t.Fatal("no ContentModerated event")
	}
}

// TestModerationWait：输出审核持续不可用时，Run 以不含正文的 RunSuspended 挂起，退避逐次翻倍、至多
// ModerationRetryMax；退避未到期时被唤醒只重新停放、不写事件；从第一次挂起起超过 ModerationWaitMax 后以明确的
// 原因失败（ADR-0054）。审核不可用期间不计入 StepErrorBudget。
func TestModerationWait(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	q := memqueue.New(clk)
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any", Capabilities: []string{"clock_now"}})
	gw := model.NewGateway()
	gw.Register("v", violating{})
	w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw, Moderator: &toggling{fail: true},
		ModerationRetry: time.Second, ModerationRetryMax: 4 * time.Second, ModerationWaitMax: 10 * time.Second,
		StepErrorBudget: time.Millisecond,
		Catalog:         &capability.Catalog{Local: capability.NewRegistry(capability.ClockNow(clk))}})

	st := &session.State{SessionID: "s1"}
	created := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "v", Version: "1"}}}}
	if err := store.Commit(ctx, st, created, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("hi")}}}); err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(ctx, "s1", workqueue.Class{})
	steps := func() {
		for range 4 {
			if _, err := w.Step(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	suspensions := func() []*v1.RunSuspended {
		events, _ := eventlog.ReadAll(ctx, store.Log, "s1", 0)
		var out []*v1.RunSuspended
		for _, e := range events {
			if e.GetAssistantMessage() != nil {
				t.Fatal("output committed while moderation was unavailable")
			}
			if s := e.GetRunSuspended(); s != nil {
				if s.GetReason() != session.SuspendModeration {
					t.Fatalf("suspended for %q", s.GetReason())
				}
				out = append(out, s)
			}
		}
		return out
	}

	// 退避：1s、2s、4s、4s（上限）。
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		start := clk.Now()
		steps()
		got := suspensions()
		if len(got) != i+1 {
			t.Fatalf("after wait %d: %d suspensions", i, len(got))
		}
		if until := got[i].GetUntil().AsTime(); !until.Equal(start.Add(want)) {
			t.Fatalf("wait %d until %s, want +%s", i, until.Sub(start), want)
		}
		if i == 0 {
			// 退避未到期时被唤醒：重新停放，不写事件、不开始 Attempt。
			before, _ := store.Load(ctx, "s1")
			_ = q.Enqueue(ctx, "s1", workqueue.Class{})
			steps()
			after, _ := store.Load(ctx, "s1")
			if after.Seq != before.Seq {
				t.Fatalf("early wake wrote %d events", after.Seq-before.Seq)
			}
			if r := after.Run("r1"); r.QuotaSuspended() || !r.ModerationSuspended() || r.ModerationWaits != 1 {
				t.Fatalf("run %+v", r)
			}
		}
		clk.Advance(want)
	}
	// 从第一次挂起起已过 11s，超过 10s 的上限：下一次不可用时失败。
	steps()
	final, _ := store.Load(ctx, "s1")
	if r := final.Run("r1"); r.Status != session.RunFailed {
		t.Fatalf("run %s, want failed", r.Status)
	}
	events, _ := eventlog.ReadAll(ctx, store.Log, "s1", 0)
	if f := events[len(events)-1].GetRunFailed(); f == nil || f.GetReason() != "moderation unavailable" {
		t.Fatalf("last event %v, want RunFailed(moderation unavailable)", events[len(events)-1])
	}
}
