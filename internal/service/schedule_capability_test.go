package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/model"
	"yanshi/internal/schedule"
)

func TestScheduleCapability(t *testing.T) {
	ctx := context.Background()
	s, clk, sid := scheduleService(t, "Asia/Shanghai")
	st, err := s.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	cp := s.ScheduleCapability()
	inv := capability.Invocation{SessionID: sid, CallID: "c1", State: st, Arguments: `{"action":"create","input":"给我一份周报","rrule":"FREQ=WEEKLY;BYDAY=FR;BYHOUR=17;BYMINUTE=0"}`}
	if !json.Valid(cp.Spec().InputSchema) {
		t.Fatal("invalid schema")
	}
	if err := cp.(capability.Checker).Check(ctx, inv); err != nil {
		t.Fatal(err)
	}
	out, err := cp.Invoke(ctx, inv)
	if err != nil || !strings.Contains(model.Text(out), "10月30日星期五 17:00（Asia/Shanghai）") {
		t.Fatalf("create %q %v", model.Text(out), err)
	}
	items, err := s.ListSchedules(ctx, sid)
	if err != nil || len(items) != 1 || items[0].Created.GetBy() != "agent" {
		t.Fatalf("list %v %v", items, err)
	}
	id := items[0].Created.GetScheduleId()
	clk.Advance(4 * 24 * time.Hour)
	if _, err := cp.Invoke(ctx, inv); err != nil {
		t.Fatal(err)
	}
	items, _ = s.ListSchedules(ctx, sid)
	if len(items) != 1 || items[0].Created.GetScheduleId() != id {
		t.Fatal("duplicate schedule")
	}
	inv.Arguments = `{"action":"list"}`
	out, err = cp.Invoke(ctx, inv)
	if err != nil || !strings.Contains(model.Text(out), id) || !strings.Contains(model.Text(out), "FREQ=WEEKLY") {
		t.Fatalf("list %q %v", model.Text(out), err)
	}
	inv.Arguments = `{"action":"cancel","schedule_id":"` + id + `"}`
	for range 2 {
		if _, err := cp.Invoke(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	inv.Arguments = `{"action":"list"}`
	out, err = cp.Invoke(ctx, inv)
	if err != nil || model.Text(out) != "没有定时" {
		t.Fatalf("empty %q %v", model.Text(out), err)
	}
}

func TestScheduleCapabilityInvalid(t *testing.T) {
	s, _, sid := scheduleService(t, "")
	st, err := s.Load(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	check := s.ScheduleCapability().(capability.Checker)
	for _, args := range []string{
		`{`, `{"action":"other"}`, `{"action":"cancel"}`,
		`{"action":"create","input":"提醒","at":"2026-10-30T17:00"}`,
		`{"action":"create","input":"提醒","time_zone":"UTC","rrule":"FREQ=DAILY"}`,
		`{"action":"create","input":"提醒","time_zone":"UTC","at":"2026-10-30T17:00","until":"bad"}`,
	} {
		if err := check.Check(context.Background(), capability.Invocation{State: st, Arguments: args}); !errors.Is(err, schedule.ErrInvalid) {
			t.Fatalf("%s: %v", args, err)
		}
	}
	st.Created.Parent = &v1.ParentRef{SessionId: "ses_v1_000_000_parent"}
	if err := check.Check(context.Background(), capability.Invocation{State: st, Arguments: `{"action":"create","input":"提醒","time_zone":"UTC","at":"2026-10-30T17:00"}`}); err == nil || !strings.Contains(err.Error(), "子任务中不能创建定时") {
		t.Fatalf("child %v", err)
	}
}

func TestScheduleCapabilityModerates(t *testing.T) {
	s, _, sid := scheduleService(t, "UTC")
	s.Moderator = scheduleBlocker{}
	st, err := s.Load(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ScheduleCapability().Invoke(context.Background(), capability.Invocation{SessionID: sid, CallID: "c", State: st, Arguments: `{"action":"create","input":"提醒","at":"2026-10-30T17:00"}`})
	if err == nil {
		t.Fatal("moderation bypassed")
	}
	items, err := s.ListSchedules(context.Background(), sid)
	if err != nil || len(items) != 0 {
		t.Fatalf("schedules %v %v", items, err)
	}
}
