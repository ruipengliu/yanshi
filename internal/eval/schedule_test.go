package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

func TestOffsetClock(t *testing.T) {
	c := &offsetClock{}
	before := time.Now()
	if now := c.Now(); now.Before(before) || now.After(time.Now()) {
		t.Fatalf("initial time %v", now)
	}
	target := before.Add(7 * 24 * time.Hour)
	c.AdvanceTo(target)
	if c.Now().Before(target) {
		t.Fatal("did not advance")
	}
	prev := c.Now()
	c.AdvanceTo(before)
	if c.Now().Before(prev) {
		t.Fatal("clock went backwards")
	}
	c.mu.Lock()
	offset := c.offset
	c.mu.Unlock()
	before = time.Now()
	if now := c.Now(); now.Before(before.Add(offset)) || now.After(time.Now().Add(offset)) {
		t.Fatal("clock no longer follows real time")
	}
}

func TestOffsetClockConcurrent(t *testing.T) {
	c := &offsetClock{}
	base := time.Now().Add(24 * time.Hour)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev := c.Now()
			for range 100 {
				c.AdvanceTo(base.Add(time.Duration(i) * time.Hour))
				now := c.Now()
				if now.Before(prev) {
					t.Error("concurrent clock went backwards")
				}
				prev = now
			}
		}()
	}
	wg.Wait()
	if c.Now().Before(base.Add(31 * time.Hour)) {
		t.Fatal("lost furthest advance")
	}
}

func TestLoadAdvanceSchedule(t *testing.T) {
	for _, extra := range []string{"input: hello", "scheduled: true", "await_background: true", "new_session: true", "card_action: {}", "ui_context: {}", "from_device: pc", "captures_file: missing.yaml", "captures: [{speaker: wearer, text: hello}]", "xinli_edit: [{gold: x}]"} {
		t.Run(extra, func(t *testing.T) {
			dir := t.TempDir()
			writeCase(t, dir, "invalid", "name: invalid\nturns:\n  - advance_to_next_schedule: true\n    "+extra+"\n")
			if _, err := LoadSuite(dir); err == nil || !strings.Contains(err.Error(), "advance_to_next_schedule") {
				t.Fatalf("wanted conflicting turn error, got %v", err)
			}
		})
	}
	dir := t.TempDir()
	writeCase(t, dir, "valid", "name: valid\ntime_zone: Asia/Shanghai\nturns:\n  - advance_to_next_schedule: true\n")
	cases, err := LoadSuite(dir)
	if err != nil || !cases[0].Turns[0].AdvanceToNextSchedule || cases[0].TimeZone != "Asia/Shanghai" {
		t.Fatalf("parse: %v, %v", cases, err)
	}
}

func scheduleConfig(t *testing.T) Config {
	t.Helper()
	defs, err := agentdef.NewRegistry(&agentdef.Def{Name: "assistant", Version: "1", Model: "script/any", Capabilities: []string{"schedule", "clock_now"}})
	if err != nil {
		t.Fatal(err)
	}
	gw := model.NewGateway()
	gw.Register("script", script{})
	cfg := Config{Agents: defs, Model: gw, Parallel: 2, TurnTimeout: 10 * time.Second}
	cfg.defaults()
	return cfg
}

func TestAdvanceScheduleFlow(t *testing.T) {
	c := &Case{Name: "advance", Agent: "assistant", TimeZone: "Asia/Shanghai", Trials: 3, Turns: []Turn{
		{Input: `call schedule {"action":"create","input":"周报内容","rrule":"FREQ=WEEKLY;BYDAY=FR;BYHOUR=17;BYMINUTE=0"}`, Expect: Expect{Calls: []string{"schedule"}}},
		{AdvanceToNextSchedule: true, Expect: Expect{ReplyContains: []string{"【定时", "周报内容"}, NoCalls: []string{"schedule"}}},
		{AdvanceToNextSchedule: true, Expect: Expect{ReplyContains: []string{"【定时", "周报内容"}}},
		{Input: "一次性提醒", Scheduled: true, Expect: Expect{ReplyContains: []string{"【定时", "一次性提醒"}}},
	}}
	// 多个 trial 各拨两次，夹带普通 trial，检查偏移不污染其他实例。
	plain := &Case{Name: "plain", Agent: "assistant", Trials: 2, Turns: []Turn{{Input: "hello", Expect: Expect{ReplyContains: []string{"hello"}}}}}
	rep, err := Run(t.Context(), scheduleConfig(t), []*Case{c, plain})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Cases {
		if r.Passed != r.Trials {
			t.Fatalf("trial failed: %+v", r)
		}
	}
}

func TestAdvanceWithoutSchedule(t *testing.T) {
	cfg := scheduleConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &Case{Name: "empty", Agent: "assistant"}
	in, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	sid, err := in.svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "empty", Agent: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.advanceToNextSchedule(ctx, sid); err == nil || !strings.Contains(err.Error(), "no active schedule") {
		t.Fatalf("expected case error, got %v", err)
	}
}

func TestScheduleAssertions(t *testing.T) {
	one := 1
	e := Expect{CallSequence: []CallMatch{{Capability: "schedule", Contains: []string{`"action":"list"`}}, {Capability: "schedule", Contains: []string{`"action":"cancel"`}}}, Schedules: &ScheduleExpect{Active: &Range{Min: &one, Max: &one}, RRuleContains: []string{"BYDAY=FR", "BYHOUR=17"}, TimeZone: "Asia/Shanghai"}}
	o := &observation{calls: []string{"schedule", "clock_now", "schedule"}, callArgs: []string{`{"action": "list"}`, `{}`, `{"action": "cancel"}`}, schedules: []*session.Schedule{{Created: &v1.ScheduleCreated{Rrule: "FREQ=WEEKLY;BYDAY=FR;BYHOUR=17;BYMINUTE=0", TimeZone: "Asia/Shanghai"}}}}
	check := func(want bool) {
		t.Helper()
		all := true
		checkSchedules(func(_ string, pass bool, _ string) { all = all && pass }, e, o)
		if all != want {
			t.Fatalf("assertions passed = %v, want %v", all, want)
		}
	}
	check(true)
	o.callArgs[0], o.callArgs[2] = o.callArgs[2], o.callArgs[0]
	check(false)
	o.callArgs[0], o.callArgs[2] = o.callArgs[2], o.callArgs[0]
	o.schedules[0].Created.TimeZone = "UTC"
	check(false)
	o.schedules = nil
	check(false)
}

func TestCaseTimeZone(t *testing.T) {
	for _, tc := range []struct{ agent, explicit, want string }{
		{"xinli-assistant", "", "Asia/Shanghai"}, {"assistant", "", ""}, {"xinli-assistant", "Europe/London", "Europe/London"},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			if got := caseTimeZone(&Case{TimeZone: tc.explicit}, tc.agent); got != tc.want {
				t.Fatalf("time zone %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScheduleClockIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := scheduleConfig(t)
	c := &Case{Name: "clock", Agent: "assistant"}
	first, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	first.clock.AdvanceTo(time.Now().Add(7 * 24 * time.Hour))
	if second.clock.Now().After(time.Now().Add(time.Minute)) {
		t.Fatal("another instance inherited the clock offset")
	}
	if first.svc.Store.Clock != first.clock || second.svc.Store.Clock != second.clock {
		t.Fatal("service does not share its instance clock")
	}
}

func TestScheduledRunEvents(t *testing.T) {
	var events []*v1.Event
	for _, id := range []string{"first", "second"} {
		events = append(events,
			&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: id}}},
			&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: id, Content: model.TextBlocks(id), ToolCalls: []*v1.ToolCall{{Capability: id, ArgumentsJson: "{}"}}}}},
			&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: id, Content: model.TextBlocks(id)}}},
			&v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{RunId: id}}},
			&v1.Event{Payload: &v1.Event_ApprovalDecided{ApprovalDecided: &v1.ApprovalDecided{RunId: id}}},
			&v1.Event{Payload: &v1.Event_ContextCompacted{ContextCompacted: &v1.ContextCompacted{RunId: id}}},
			&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: id}}},
		)
	}
	got := scheduledRunEvents(events, "first")
	o := &observation{}
	collect(got, o)
	if len(got) != 7 || o.reply != "first" || len(o.calls) != 1 || o.calls[0] != "first" || o.approvals != 1 || o.compactions != 1 || strings.Contains(strings.Join(o.steps, "\n"), "second") {
		t.Fatalf("another schedule leaked into observations: %+v", o)
	}
}

func TestAdvanceIgnoresCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	in, err := start(ctx, scheduleConfig(t), []*Case{{Name: "cancelled", Agent: "assistant"}})
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	sid, err := in.svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "cancelled", Agent: "assistant", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	due := in.clock.Now().Add(time.Hour).UTC().Truncate(time.Second)
	item, err := in.svc.CreateSchedule(ctx, sid, service.CreateScheduleRequest{Input: model.TextBlocks("once"), At: due.Format("2006-01-02T15:04:05"), TimeZone: "UTC", By: "service"})
	if err != nil {
		t.Fatal(err)
	}
	// 取消留下历史声明，但不能被当成下一次有效到期。
	if err := in.svc.CancelSchedule(ctx, sid, item.Created.GetScheduleId(), "service"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.advanceToNextSchedule(ctx, sid); err == nil || !strings.Contains(err.Error(), "no active schedule") {
		t.Fatalf("cancelled schedule was selected: %v", err)
	}
}
