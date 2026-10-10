package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/askuser"
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

// TestAnswer：回答 ask_user 提问（ADR-0025）。不合法的回答、违规的文字被拒绝且不写日志；合法的回答作为外部结果写入；
// 已回答的提问再回答是冲突；Run 等回答时提交的输入作为回答而不是插话。
func TestAnswer(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents, Moderator: moderation.Mock{}}
	sid, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	res, err := svc.Submit(ctx, sid, model.TextBlocks("约个会"))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 Worker：提问并挂起。
	ask := func(callID string) {
		t.Helper()
		st, _ := svc.Load(ctx, sid)
		attempt := st.Active().Attempt + 1
		err := store.Commit(ctx, st,
			&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: res.RunID, Attempt: attempt}}},
			&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: res.RunID, Attempt: attempt,
				ToolCalls: []*v1.ToolCall{{CallId: callID, Capability: askuser.Capability,
					ArgumentsJson: `{"question":"哪天？","options":[{"id":"wed","label":"周三"},{"id":"fri","label":"周五"}]}`}}}}},
			&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: res.RunID, Attempt: attempt,
				CallId: callID, NodeId: askuser.NodeID}}},
			&v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: &v1.RunSuspended{RunId: res.RunID, Attempt: attempt}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	ask("q1")
	head := func() uint64 { st, _ := svc.Load(ctx, sid); return st.Seq }
	before := head()
	for name, c := range map[string]struct {
		call string
		a    askuser.Answer
		want error
	}{
		"unknown option": {"q1", askuser.Answer{Selected: []string{"sun"}}, service.ErrInvalid},
		"two options":    {"q1", askuser.Answer{Selected: []string{"wed", "fri"}}, service.ErrInvalid},
		"empty":          {"q1", askuser.Answer{}, service.ErrInvalid},
		"no question":    {"q9", askuser.Answer{Text: "x"}, service.ErrNotFound},
		"blocked text":   {"q1", askuser.Answer{Text: "【违规测试】"}, moderation.ErrRejected},
	} {
		if err := svc.Answer(ctx, sid, c.call, &c.a); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if head() != before {
		t.Fatal("a rejected answer was written to the log")
	}
	if err := svc.Answer(ctx, sid, "q1", &askuser.Answer{Selected: []string{"fri"}}); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Load(ctx, sid)
	last := st.History[len(st.History)-1].GetToolResult()
	if last.GetCallId() != "q1" || last.GetAttempt() != 0 || model.Text(last.GetContent()) != `{"selected":[{"id":"fri","label":"周五"}]}` {
		t.Fatalf("answer recorded as %v", last)
	}
	if err := svc.Answer(ctx, sid, "q1", &askuser.Answer{Selected: []string{"wed"}}); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("second answer: %v", err)
	}

	// 打字回答：Run 等回答时，输入作为回答。
	ask("q2")
	sub, err := svc.SubmitWithID(ctx, sid, "typed-1", model.TextBlocks("下周一吧"))
	if err != nil || sub.Answered != "q2" || sub.RunID != res.RunID {
		t.Fatalf("typed answer: %+v %v", sub, err)
	}
	if again, err := svc.SubmitWithID(ctx, sid, "typed-1", model.TextBlocks("下周一吧")); err != nil || !again.Duplicate || again.Answered != "q2" {
		t.Fatalf("retry of a typed answer: %+v %v", again, err)
	}
	st, _ = svc.Load(ctx, sid)
	if r := st.History[len(st.History)-1].GetToolResult(); r.GetCallId() != "q2" || model.Text(r.GetContent()) != `{"text":"下周一吧"}` {
		t.Fatalf("typed answer recorded as %v", st.History[len(st.History)-1])
	}
	// 没有待回答的提问时，输入照常是插话。
	sub, err = svc.Submit(ctx, sid, model.TextBlocks("另外记得订会议室"))
	if err != nil || sub.Answered != "" || !sub.Steered {
		t.Fatalf("steer: %+v %v", sub, err)
	}
}

// TestUIContextLimits：每条输入至多一个界面上下文，字段不超过上限，且须伴随用户的话；违规的界面内容同样被拒绝。
func TestUIContextLimits(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents, Moderator: moderation.Mock{}}
	sid, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	ui := func(u *v1.UIContext) *v1.ContentBlock {
		return &v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: u}}
	}
	text := model.TextBlocks("这个是什么？")
	for name, c := range map[string]struct {
		input []*v1.ContentBlock
		want  error
	}{
		"only ui context": {[]*v1.ContentBlock{ui(&v1.UIContext{Screen: "s"})}, service.ErrInvalid},
		"two":             {append([]*v1.ContentBlock{ui(&v1.UIContext{}), ui(&v1.UIContext{})}, text...), service.ErrInvalid},
		"content too long": {append([]*v1.ContentBlock{ui(&v1.UIContext{Content: strings.Repeat("长", service.MaxUIContextContent+1)})}, text...),
			service.ErrInvalid},
		"blocked content": {append([]*v1.ContentBlock{ui(&v1.UIContext{Content: "【违规测试】"})}, text...), moderation.ErrRejected},
	} {
		if _, err := svc.Submit(ctx, sid, c.input); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	ok := append([]*v1.ContentBlock{ui(&v1.UIContext{Screen: "订单详情", Content: strings.Repeat("长", service.MaxUIContextContent)})}, text...)
	if _, err := svc.Submit(ctx, sid, ok); err != nil {
		t.Fatal(err)
	}
}

// TestSubmitWithIDIsIdempotent：同一输入 ID 的重复提交返回首次的结果，不再写入（新 Run、插话、打字回答都如此）；
// 去重窗口之外的旧 ID 不再去重。
func TestSubmitWithIDIsIdempotent(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents}
	sid, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	head := func() uint64 { st, _ := svc.Load(ctx, sid); return st.Seq }
	submit := func(id, text string) *service.SubmitResult {
		t.Helper()
		res, err := svc.SubmitWithID(ctx, sid, id, model.TextBlocks(text))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := submit("in-1", "你好")
	before := head()
	again := submit("in-1", "你好")
	if !again.Duplicate || again.RunID != first.RunID || again.Steered || head() != before {
		t.Fatalf("retry of a new run: %+v (first %+v), log %d→%d", again, first, before, head())
	}
	steer := submit("in-2", "补充一句")
	if !steer.Steered || steer.Duplicate {
		t.Fatalf("steer: %+v", steer)
	}
	if again := submit("in-2", "补充一句"); !again.Duplicate || !again.Steered || again.RunID != first.RunID {
		t.Fatalf("retry of a steer: %+v", again)
	}
	// 没有 ID 的提交不去重。
	before = head()
	submit("", "x")
	submit("", "x")
	if head() != before+2 {
		t.Fatal("inputs without an id were deduplicated")
	}
	// 窗口之外的旧 ID 不再去重。
	for i := range session.MaxRecentInputs {
		submit(fmt.Sprintf("fill-%d", i), "x")
	}
	if res := submit("in-1", "你好"); res.Duplicate {
		t.Fatal("an id outside the window was still deduplicated")
	}
	if _, err := svc.SubmitWithID(ctx, sid, strings.Repeat("x", service.MaxInputIDLen+1), text("x")); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("overlong input id: %v", err)
	}
}

func text(s string) []*v1.ContentBlock { return model.TextBlocks(s) }
