package service_test

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

// TestWithdrawnVersionSwitchesAtNextRun：撤回版本后，新 Session 不再使用它、也不能指定它；
// 已有 Session 的进行中 Run 不受影响（插话不切换），下一个 Run 开始时切换到稳定版本。
func TestWithdrawnVersionSwitchesAtNextRun(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"}, &agentdef.Def{Name: "a", Version: "2", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents}
	release := func(s string) {
		rel, err := agents.ParseReleases([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		agents.SetReleases(rel)
	}
	release(`a: {stable: "1", canary: {version: "2", percent: 0, end_users: [qa]}}`)
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "qa", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Load(ctx, sid)
	if st.Agent.GetVersion() != "2" {
		t.Fatalf("allowlisted user got version %s, want canary 2", st.Agent.GetVersion())
	}
	first, err := svc.Submit(ctx, sid, model.TextBlocks("hi"))
	if err != nil {
		t.Fatal(err)
	}

	release(`a: {stable: "1", withdrawn: ["2"]}`)
	if _, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "x", Agent: "a", AgentVersion: "2"}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("created a session on a withdrawn version: %v", err)
	}
	if res, err := svc.Submit(ctx, sid, model.TextBlocks("more")); err != nil || !res.Steered {
		t.Fatalf("steer: %+v %v", res, err)
	}
	if st, _ := svc.Load(ctx, sid); st.Agent.GetVersion() != "2" {
		t.Fatal("active run switched versions mid-run")
	}
	if err := svc.Interrupt(ctx, sid, first.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, sid, model.TextBlocks("again")); err != nil {
		t.Fatal(err)
	}
	st, _ = svc.Load(ctx, sid)
	if st.Agent.GetVersion() != "1" {
		t.Fatalf("next run on version %s, want stable 1", st.Agent.GetVersion())
	}
	events, _ := eventlog.ReadAll(ctx, store.Log, sid, 0)
	var switches int
	for _, e := range events {
		if s := e.GetAgentSwitched(); s != nil {
			switches++
			if s.GetFrom().GetVersion() != "2" || s.GetTo().GetVersion() != "1" || s.GetReason() != "withdrawn" {
				t.Fatalf("switch event %v", s)
			}
		}
	}
	if switches != 1 {
		t.Fatalf("%d switch events, want 1", switches)
	}
}

// TestInvalidSwitchesAreRejected：切换只能在 Run 之间、同名、且 from 与当前版本一致。
func TestInvalidSwitchesAreRejected(t *testing.T) {
	ref := func(n, v string) *v1.AgentRef { return &v1.AgentRef{Name: n, Version: v} }
	ev := func(seq uint64, p any) *v1.Event {
		e := &v1.Event{SessionId: "s", Seq: seq}
		switch p := p.(type) {
		case *v1.SessionCreated:
			e.Payload = &v1.Event_SessionCreated{SessionCreated: p}
		case *v1.RunRequested:
			e.Payload = &v1.Event_RunRequested{RunRequested: p}
		case *v1.AgentSwitched:
			e.Payload = &v1.Event_AgentSwitched{AgentSwitched: p}
		}
		return e
	}
	created := ev(1, &v1.SessionCreated{Agent: ref("a", "1")})
	for name, events := range map[string][]*v1.Event{
		"during run": {created, ev(2, &v1.RunRequested{RunId: "r"}), ev(3, &v1.AgentSwitched{From: ref("a", "1"), To: ref("a", "2")})},
		"wrong from": {created, ev(2, &v1.AgentSwitched{From: ref("a", "3"), To: ref("a", "2")})},
		"other name": {created, ev(2, &v1.AgentSwitched{From: ref("a", "1"), To: ref("b", "1")})},
	} {
		if _, err := session.Reduce(events); err == nil {
			t.Errorf("%s: switch accepted", name)
		}
	}
}

type failingModerator struct{}

func (failingModerator) Check(context.Context, moderation.Request) (moderation.Verdict, error) {
	return moderation.Verdict{}, errors.New("timeout")
}

// TestInputModeration：违规输入（新 Run 与插话）被拒绝且不写入日志；内容安全服务不可用时不放行。
func TestInputModeration(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents, Moderator: moderation.Mock{}}
	sid, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	head := func() uint64 { st, _ := svc.Load(ctx, sid); return st.Seq }
	before := head()
	if _, err := svc.Submit(ctx, sid, model.TextBlocks("帮我写【违规测试】")); !errors.Is(err, moderation.ErrRejected) {
		t.Fatalf("blocked input accepted: %v", err)
	}
	if head() != before {
		t.Fatal("blocked input was written to the log")
	}
	if _, err := svc.Submit(ctx, sid, model.TextBlocks("你好")); err != nil {
		t.Fatal(err)
	}
	// 插话同样检查。
	before = head()
	if _, err := svc.Submit(ctx, sid, model.TextBlocks("[moderation-test]")); !errors.Is(err, moderation.ErrRejected) || head() != before {
		t.Fatalf("blocked steer: %v", err)
	}
	svc.Moderator = failingModerator{}
	if _, err := svc.Submit(ctx, sid, model.TextBlocks("你好")); !errors.Is(err, moderation.ErrUnavailable) || head() != before {
		t.Fatalf("input passed while moderation was unavailable: %v", err)
	}
}
