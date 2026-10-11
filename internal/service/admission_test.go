package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

type fakeStats struct{ depths []workqueue.Depth }

func (f *fakeStats) Depths(context.Context) ([]workqueue.Depth, error) { return f.depths, nil }

func interactiveWait(bl string, d time.Duration) []workqueue.Depth {
	return []workqueue.Depth{{Class: workqueue.Class{BusinessLine: bl, Priority: workqueue.Interactive}, Ready: 3, OldestWait: d},
		{Class: workqueue.Class{BusinessLine: bl, Priority: workqueue.Background}, Ready: 100, OldestWait: time.Hour}}
}

// 交互工作等得太久时拒绝新 Run，回落到恢复阈值以下才恢复（滞回）；后台积压不影响；只拦积压的业务线。
func TestQueueAdmissionHysteresis(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	stats := &fakeStats{depths: interactiveWait("bl", time.Second)}
	a := &service.QueueAdmission{Stats: stats, MaxWait: 10 * time.Second, ResumeWait: 3 * time.Second, Clock: clk}
	step := func(wait time.Duration, want bool) {
		t.Helper()
		stats.depths = interactiveWait("bl", wait)
		clk.Advance(3 * time.Second) // 超过 Refresh，重新统计
		err := a.Admit(ctx, "bl")
		if rejected := errors.Is(err, service.ErrOverloaded); rejected != want {
			t.Fatalf("wait %s: rejected = %v, want %v", wait, rejected, want)
		}
		if err := a.Admit(ctx, "other"); err != nil {
			t.Fatalf("other business line rejected: %v", err)
		}
	}
	step(time.Second, false)
	step(11*time.Second, true)
	step(5*time.Second, true) // 仍高于恢复阈值
	step(2*time.Second, false)
	step(5*time.Second, false) // 低于开始拒绝的阈值
}

// 准入只拦新 Run：进行中 Run 的插话照常接受。
func TestAdmissionRejectsNewRunsOnly(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	mlog, mq := memlog.New(), memqueue.New(clk)
	store := &session.Store{Log: mlog, Waking: mlog.Waking(mq), IDs: ids.Sequential("id"), Clock: clk}
	stats := &fakeStats{}
	svc := &service.Service{Store: store, Agents: agents,
		Admission: &service.QueueAdmission{Stats: stats, MaxWait: 10 * time.Second, ResumeWait: 5 * time.Second, Clock: clk}}
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, sid, model.TextBlocks("first")); err != nil {
		t.Fatal(err)
	}
	stats.depths = interactiveWait("bl", time.Minute)
	clk.Advance(3 * time.Second)
	res, err := svc.Submit(ctx, sid, model.TextBlocks("steer"))
	if err != nil || !res.Steered {
		t.Fatalf("steer during overload: %+v, %v", res, err)
	}
	other, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u2", Agent: "a"})
	var ov *service.OverloadedError
	if _, err := svc.Submit(ctx, other, model.TextBlocks("new run")); !errors.As(err, &ov) || !ov.RetryAt.After(clk.Now()) {
		t.Fatalf("new run during overload: %v", err)
	}
}

// 移出投影的旧 Run 仍可按 ID 中断（无操作），重复的审批仍返回 conflict；不存在的才是 not found。
func TestTrimmedRunsKeepTheirAnswers(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	mlog, mq := memlog.New(), memqueue.New(clock.Real{})
	store := &session.Store{Log: mlog, Waking: mlog.Waking(mq), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Agents: agents}
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := range session.KeepRuns + 3 {
		res, err := svc.Submit(ctx, sid, model.TextBlocks("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = res.RunID
			st, _ := svc.Load(ctx, sid)
			call := "call_old"
			if err := store.Commit(ctx, st,
				&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: first, Attempt: 1}}},
				&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: first, Attempt: 1,
					ToolCalls: []*v1.ToolCall{{CallId: call, Capability: "pc__send"}}}}},
				&v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{RunId: first, Attempt: 1, CallId: call}}},
			); err != nil {
				t.Fatal(err)
			}
		}
		if err := svc.Interrupt(ctx, sid, res.RunID); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := svc.Load(ctx, sid); st.Run(first) != nil {
		t.Fatal("first run was not trimmed from the projection")
	}
	if err := svc.Interrupt(ctx, sid, first); err != nil {
		t.Fatalf("interrupt of a trimmed run: %v", err)
	}
	if err := svc.Interrupt(ctx, sid, "run_nope"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("interrupt of an unknown run: %v", err)
	}
	if err := svc.Decide(ctx, sid, "call_old", true, "u"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("decision on a trimmed run's call: %v", err)
	}
	if err := svc.Decide(ctx, sid, "call_nope", true, "u"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("decision on an unknown call: %v", err)
	}
}
