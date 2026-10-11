package service_test

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/card"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

// TestCardActions：卡片动作按日志中的那一版卡片核对，标签由平台填写；同一版卡片上的同一动作只生效一次；Run 进行中时
// 并入它，Run 结束后开启新 Run；用户输入中不能带卡片本身（docs/design/cards.md §4）。
func TestCardActions(t *testing.T) {
	ctx := context.Background()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	mlog, mq := memlog.New(), memqueue.New(clock.Real{})
	store := &session.Store{Log: mlog, Waking: mlog.Waking(mq), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Agents: agents, Moderator: moderation.Mock{}}
	sid, _ := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	res, err := svc.Submit(ctx, sid, model.TextBlocks("明早七点叫我"))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 Worker：调用 show_card 并写入结果（卡片随结果写入日志）。
	st, _ := svc.Load(ctx, sid)
	c, err := card.Parse(ctx, `{"card_id":"alarm","title":"闹钟 07:00","body":[{"type":"field","id":"repeat","label":"重复","input":"select","options":["不重复","工作日"]}],
		"buttons":[{"id":"ok","label":"好的"},{"id":"cancel","label":"取消闹钟","style":"danger"}]}`, "call_card", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Commit(ctx, st,
		&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: res.RunID, Attempt: 1}}},
		&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: res.RunID, Attempt: 1,
			ToolCalls: []*v1.ToolCall{{CallId: "call_card", Capability: card.Capability, ArgumentsJson: "{}"}}}}},
		&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: res.RunID, Attempt: 1, CallId: "call_card"}}},
		&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: res.RunID, Attempt: 1, CallId: "call_card",
			Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: c}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	cardSeq := st.Seq
	// 卡片只能写在 show_card 的结果中（session.Apply 校验）：其他调用的结果、用户输入中的卡片都被拒绝。
	st, _ = svc.Load(ctx, sid)
	forged := []*v1.Event{
		{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: res.RunID, Attempt: 1,
			ToolCalls: []*v1.ToolCall{{CallId: "call_x", Capability: "clock_now", ArgumentsJson: "{}"}}}}},
		{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: res.RunID, Attempt: 1, CallId: "call_x"}}},
		{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: res.RunID, Attempt: 1, CallId: "call_x",
			Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: c}}}}}},
	}
	if err := store.Commit(ctx, st, forged...); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("card in a clock_now result: %v", err)
	}
	if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: res.RunID,
		Input: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: c}}}}}}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("card in user input: %v", err)
	}
	action := func(act string, seq uint64, values map[string]string) []*v1.ContentBlock {
		return []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: &v1.CardAction{CardId: "alarm", CallId: "call_card",
			Action: act, Label: "伪造的标签", Values: values, Seq: seq}}}}
	}
	head := func() uint64 { st, _ := svc.Load(ctx, sid); return st.Seq }
	before := head()
	for name, c := range map[string]struct {
		input []*v1.ContentBlock
		want  error
	}{
		"unknown action":  {action("delete", cardSeq, nil), service.ErrInvalid},
		"unknown field":   {action("ok", cardSeq, map[string]string{"x": "1"}), service.ErrInvalid},
		"bad option":      {action("ok", cardSeq, map[string]string{"repeat": "每天"}), service.ErrInvalid},
		"unknown card":    {[]*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: &v1.CardAction{CardId: "x", CallId: "call_card", Action: "ok"}}}}, service.ErrNotFound},
		"forged card":     {[]*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: c}}}, service.ErrInvalid},
		"two actions":     {append(action("ok", cardSeq, nil), action("cancel", cardSeq, nil)...), service.ErrInvalid},
		"blocked by text": {append(action("ok", cardSeq, nil), model.TextBlocks("【违规测试】")...), moderation.ErrRejected},
	} {
		if _, err := svc.Submit(ctx, sid, c.input); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if head() != before {
		t.Fatal("a rejected card action was written to the log")
	}

	// Run 进行中：动作并入它；标签取自卡片；重复点击（含不带序号、需扫描日志的重试）只生效一次。
	sub, err := svc.Submit(ctx, sid, action("ok", cardSeq, map[string]string{"repeat": "工作日"}))
	if err != nil || !sub.Steered || sub.RunID != res.RunID {
		t.Fatalf("steer: %+v %v", sub, err)
	}
	st, _ = svc.Load(ctx, sid)
	got := st.History[len(st.History)-1].GetSteered().GetInput()[0].GetCardAction()
	if got.GetLabel() != "好的" || got.GetValues()["repeat"] != "工作日" {
		t.Fatalf("recorded action %v", got)
	}
	if again, err := svc.Submit(ctx, sid, action("ok", 0, nil)); err != nil || !again.Duplicate {
		t.Fatalf("second click: %+v %v", again, err)
	}

	// Run 结束后：另一个动作开启新 Run。
	st, _ = svc.Load(ctx, sid)
	if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: res.RunID, Attempt: 1}}}); err != nil {
		t.Fatal(err)
	}
	sub, err = svc.Submit(ctx, sid, action("cancel", cardSeq, nil))
	if err != nil || sub.Steered || sub.RunID == res.RunID {
		t.Fatalf("new run: %+v %v", sub, err)
	}
}
