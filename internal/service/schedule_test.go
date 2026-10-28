package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

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

func scheduleService(t *testing.T, zone string) (*service.Service, *clock.Fake, string) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	log, q := memlog.New(), memqueue.New(clk)
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	s := &service.Service{Store: &session.Store{Log: log, Waking: log.Waking(q), Clock: clk, IDs: ids.Sequential("s")}, Agents: agents}
	sid, err := s.Create(context.Background(), service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a", TimeZone: zone})
	if err != nil {
		t.Fatal(err)
	}
	return s, clk, sid
}

func scheduleReq(id string) service.CreateScheduleRequest {
	return service.CreateScheduleRequest{ScheduleID: id, Input: model.TextBlocks("提醒"), At: "2026-10-28T01:00:00", By: "service"}
}

func TestScheduleCreateListCancel(t *testing.T) {
	ctx := context.Background()
	s, clk, sid := scheduleService(t, "UTC")
	req := scheduleReq("one")
	item, err := s.CreateSchedule(ctx, sid, req)
	if err != nil || item.Created.GetTimeZone() != "UTC" {
		t.Fatalf("create: %v %v", item, err)
	}
	clk.Advance(2 * time.Hour)
	if _, err := s.CreateSchedule(ctx, sid, req); err != nil {
		t.Fatalf("retry after due: %v", err)
	}
	changed := req
	changed.Input = model.TextBlocks("另一个提醒")
	if _, err := s.CreateSchedule(ctx, sid, changed); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	items, err := s.ListSchedules(ctx, sid)
	if err != nil || len(items) != 1 {
		t.Fatalf("list %v %v", items, err)
	}
	if err := s.CancelSchedule(ctx, sid, "one", "end_user"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelSchedule(ctx, sid, "one", "end_user"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
	if _, err := s.CreateSchedule(ctx, sid, req); err != nil {
		t.Fatal(err)
	}
	items, _ = s.ListSchedules(ctx, sid)
	if len(items) != 0 {
		t.Fatal("retry revived cancelled schedule")
	}
}

func TestScheduleCreationChecks(t *testing.T) {
	ctx := context.Background()
	s, _, sid := scheduleService(t, "")
	req := scheduleReq("one")
	if _, err := s.CreateSchedule(ctx, sid, req); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("missing zone: %v", err)
	}
	req.TimeZone = "UTC"
	if _, err := s.CreateSchedule(ctx, sid, req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a", TimeZone: "not/a/zone"}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("session zone: %v", err)
	}
	child, err := s.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a", Parent: &v1.ParentRef{SessionId: sid, RunId: "r", CallId: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, child, req); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("child: %v", err)
	}
	for n := 1; n < session.MaxSchedules; n++ {
		req.ScheduleID = fmt.Sprintf("s%d", n)
		if _, err := s.CreateSchedule(ctx, sid, req); err != nil {
			t.Fatal(err)
		}
	}
	req.ScheduleID = "overflow"
	if _, err := s.CreateSchedule(ctx, sid, req); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("limit: %v", err)
	}
	if err := service.CheckClientInput(model.TextBlocks("提醒"), &v1.ScheduleMark{}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("client mark: %v", err)
	}
}

type scheduleBlocker struct{}

func (scheduleBlocker) Check(context.Context, moderation.Request) (moderation.Verdict, error) {
	return moderation.Verdict{Block: true}, nil
}

func TestScheduleModeration(t *testing.T) {
	s, _, sid := scheduleService(t, "UTC")
	s.Moderator = scheduleBlocker{}
	if _, err := s.CreateSchedule(context.Background(), sid, scheduleReq("one")); !errors.Is(err, moderation.ErrRejected) {
		t.Fatalf("moderation: %v", err)
	}
	st, _ := s.Load(context.Background(), sid)
	if len(st.Schedules) != 0 {
		t.Fatal("rejected schedule persisted")
	}
}

func TestScheduleFire(t *testing.T) {
	for _, kind := range []string{"once", "catchup", "closed"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, clk, sid := scheduleService(t, "UTC")
			req := scheduleReq("one")
			if kind != "once" {
				req.At = ""
				req.RRule = "FREQ=DAILY;BYHOUR=1,2,3,4;BYMINUTE=0;COUNT=4"
			}
			if _, err := s.CreateSchedule(ctx, sid, req); err != nil {
				t.Fatal(err)
			}
			if kind == "closed" {
				if err := s.Close(ctx, sid, "service", "test"); err != nil {
					t.Fatal(err)
				}
			}
			clk.Advance(3 * time.Hour)
			st, _ := s.Load(ctx, sid)
			if err := s.FireSchedule(ctx, st, "one"); err != nil {
				t.Fatal(err)
			}
			seq := st.Seq
			if err := s.FireSchedule(ctx, st, "one"); err != nil || st.Seq != seq {
				t.Fatalf("repeat: %v", err)
			}
			events, _ := eventlog.ReadAll(ctx, s.Store.Log, sid, 0)
			fired, skipped := 0, 0
			for _, e := range events {
				if e.GetScheduleFired() != nil {
					fired++
				}
				if m := e.GetScheduleSkipped(); m != nil {
					skipped += int(m.GetCount())
				}
			}
			wantFire, wantSkip := 1, 0
			if kind == "catchup" {
				wantSkip = 2
			}
			if kind == "closed" {
				wantFire, wantSkip = 0, 3
			}
			if fired != wantFire || skipped != wantSkip {
				t.Fatalf("fired %d skipped %d", fired, skipped)
			}
			if kind != "closed" {
				pieces := st.Pieces()
				if len(pieces) != 1 || pieces[0].From != session.FromSchedule {
					t.Fatalf("pieces %+v", pieces)
				}
			}
		})
	}
}

func TestScheduleWaitsForQueuedRun(t *testing.T) {
	ctx := context.Background()
	s, clk, sid := scheduleService(t, "UTC")
	if _, err := s.CreateSchedule(ctx, sid, scheduleReq("one")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Submit(ctx, sid, model.TextBlocks("先处理"))
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	st, _ := s.Load(ctx, sid)
	seq := st.Seq
	if err := s.FireSchedule(ctx, st, "one"); err != nil || st.Seq != seq {
		t.Fatalf("queued run: %v", err)
	}
	if err := s.Interrupt(ctx, sid, res.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.FireSchedule(ctx, st, "one"); err != nil {
		t.Fatal(err)
	}
	// 外部中断后使用旧投影时先无操作，Worker 的下一步 Sync 再尝试。
	st, _ = s.Load(ctx, sid)
	if err := s.FireSchedule(ctx, st, "one"); err != nil || st.Active() == nil {
		t.Fatalf("after interrupt: %v", err)
	}
}

func TestScheduleInvalidRequestsAndOverride(t *testing.T) {
	ctx := context.Background()
	s, _, sid := scheduleService(t, "Asia/Shanghai")
	req := scheduleReq("override")
	req.TimeZone = "UTC"
	item, err := s.CreateSchedule(ctx, sid, req)
	if err != nil || item.Created.GetTimeZone() != "UTC" {
		t.Fatalf("override: %v %v", item, err)
	}
	for _, kind := range []string{"past", "text", "by", "id"} {
		r := scheduleReq(kind)
		r.TimeZone = "UTC"
		switch kind {
		case "past":
			r.At = "2026-10-27T01:00:00"
		case "text":
			r.Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Media{Media: &v1.Media{}}}}
		case "by":
			r.By = "token"
		case "id":
			r.ScheduleID = "bad\nline"
		}
		if _, err := s.CreateSchedule(ctx, sid, r); !errors.Is(err, service.ErrInvalid) {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if err := s.CancelSchedule(ctx, sid, "absent", "service"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	if err := s.Close(ctx, sid, "service", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, sid, req); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("closed: %v", err)
	}
}

func TestScheduleOptimisticConflictReconsiders(t *testing.T) {
	ctx := context.Background()
	s, clk, sid := scheduleService(t, "UTC")
	if _, err := s.CreateSchedule(ctx, sid, scheduleReq("one")); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelSchedule(ctx, sid, "one", "end_user"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	if err := s.FireSchedule(ctx, stale, "one"); err != nil {
		t.Fatal(err)
	}
	if stale.Active() != nil || !stale.Schedules["one"].NextDue.IsZero() {
		t.Fatal("stale worker ignored cancellation")
	}
}

func TestScheduleFireAnchorsPersistedNextDue(t *testing.T) {
	for _, kind := range []string{"once-earlier", "once-later", "periodic-before", "periodic-after", "periodic-catchup"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, clk, sid := scheduleService(t, "UTC")
			start := clk.Now()
			due, now := start.Add(30*time.Minute), start.Add(30*time.Minute)
			m := &v1.ScheduleCreated{ScheduleId: "history", Input: model.TextBlocks("提醒"), TimeZone: "UTC", At: "2026-10-28T01:00:00", Start: timestamppb.New(start), By: "service"}
			switch kind {
			case "once-earlier":
				now = start.Add(2 * time.Hour)
			case "once-later":
				due, now = start.Add(90*time.Minute), start.Add(90*time.Minute)
			case "periodic-before", "periodic-after", "periodic-catchup":
				m.At, m.Rrule = "", "FREQ=DAILY;BYHOUR=1,2,3,4;BYMINUTE=0"
				if kind == "periodic-after" {
					due, now = start.Add(90*time.Minute), start.Add(90*time.Minute)
				}
				if kind == "periodic-catchup" {
					now = start.Add(3 * time.Hour)
				}
			}
			m.NextDue = timestamppb.New(due)
			st, err := s.Load(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}); err != nil {
				t.Fatal(err)
			}
			clk.Advance(now.Sub(start))
			if err := s.FireSchedule(ctx, st, "history"); err != nil {
				t.Fatal(err)
			}
			events, err := eventlog.ReadAll(ctx, s.Store.Log, sid, 0)
			if err != nil {
				t.Fatal(err)
			}
			var fired *v1.ScheduleFired
			var skipped *v1.ScheduleSkipped
			for _, e := range events {
				if e.GetScheduleFired() != nil {
					fired = e.GetScheduleFired()
				}
				if e.GetScheduleSkipped() != nil {
					skipped = e.GetScheduleSkipped()
				}
			}
			wantFire := due
			if kind == "periodic-catchup" {
				wantFire = now
				if skipped == nil || skipped.GetCount() != 3 || !skipped.GetFirstDue().AsTime().Equal(due) || !skipped.GetLastDue().AsTime().Equal(start.Add(2*time.Hour)) {
					t.Fatalf("skipped: %v", skipped)
				}
			} else if skipped != nil {
				t.Fatalf("unexpected skip: %v", skipped)
			}
			if fired == nil || !fired.GetDueAt().AsTime().Equal(wantFire) {
				t.Fatalf("fired: %v", fired)
			}
			if m.At != "" && fired.GetNextDue() != nil {
				t.Fatal("one-shot fired twice after tzdata shift")
			}
			if m.Rrule != "" {
				wantNext := start.Add(time.Hour)
				if kind == "periodic-after" {
					wantNext = start.Add(2 * time.Hour)
				}
				if kind == "periodic-catchup" {
					wantNext = start.Add(4 * time.Hour)
				}
				if !fired.GetNextDue().AsTime().Equal(wantNext) {
					t.Fatalf("next: %v want %s", fired.GetNextDue(), wantNext)
				}
			}
		})
	}
}
