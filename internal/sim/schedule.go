package sim

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/faults"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/schedule"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

func (w *World) scheduleStep() error {
	ctx := context.Background()
	live := w.live()
	if len(live) == 0 {
		return nil
	}
	sid := live[w.rng.IntN(len(live))]
	st, err := w.svc.Load(ctx, sid)
	if err != nil {
		return err
	}
	if st.Closed != nil {
		return nil
	}
	open := st.OpenSchedules()
	if len(open) > 0 && w.chance(0.4) {
		return w.svc.CancelSchedule(ctx, sid, open[w.rng.IntN(len(open))].Created.GetScheduleId(), "service")
	}
	w.nextInput++
	req := service.CreateScheduleRequest{ScheduleID: fmt.Sprintf("sim-sched-%d", w.nextInput), Input: model.TextBlocks("定时检查"), TimeZone: "UTC", By: "service"}
	if w.chance(0.5) {
		req.At = w.clock.Now().Add(time.Duration(1+w.rng.IntN(60)) * time.Second).UTC().Format("2006-01-02T15:04:05")
	} else {
		req.RRule = "FREQ=DAILY;BYHOUR=0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23;BYMINUTE=0;COUNT=5"
	}
	_, err = w.svc.CreateSchedule(ctx, sid, req)
	if w.submitModerated(err) {
		return nil
	}
	if st.Created.GetParent() != nil {
		if !errors.Is(err, service.ErrInvalid) {
			return fmt.Errorf("delegated schedule creation: %v", err)
		}
		w.Stats.SchedulesChildRejected++
		return nil
	}
	if errors.Is(err, service.ErrInvalid) && len(open) == session.MaxSchedules {
		return nil
	}
	return err
}

// checkSchedules 用 Next 逐次枚举，不复用写入方的 Catchup 合并算法；每一段必须从尚未处理的第一点开始。
func (w *World) checkSchedules(sid string, events []*v1.Event, st *session.State) error {
	plans := map[string]*schedule.Plan{}
	next := map[string]time.Time{}
	inputs := map[string]bool{}
	active, closed := false, false
	for i, e := range events {
		if m := e.GetScheduleCreated(); m != nil {
			p, err := schedule.Compile(schedule.Spec{At: m.GetAt(), RRule: m.GetRrule(), TimeZone: m.GetTimeZone(), Until: simScheduleTime(m.GetUntil())}, m.GetStart().AsTime())
			if err != nil {
				return err
			}
			plans[m.GetScheduleId()] = p
			next[m.GetScheduleId()], _ = p.Next(p.Start())
		}
		if m := e.GetScheduleCancelled(); m != nil {
			next[m.GetScheduleId()] = time.Time{}
		}
		if m := e.GetScheduleFired(); m != nil {
			id, due := m.GetScheduleId(), m.GetDueAt().AsTime()
			if closed || active || next[id].IsZero() || !next[id].Equal(due) || i+1 == len(events) {
				return fmt.Errorf("invariant: invalid firing %s/%s", sid, id)
			}
			r := events[i+1].GetRunRequested()
			inputID := session.ScheduleInputID(id, due)
			if r.GetSchedule().GetScheduleId() != id || !r.GetSchedule().GetDueAt().AsTime().Equal(due) || r.GetRunId() != m.GetRunId() || r.GetInputId() != inputID || inputs[inputID] {
				return fmt.Errorf("invariant: unpaired or duplicate schedule %s/%s", sid, id)
			}
			inputs[inputID] = true
			next[id], _ = plans[id].Next(due)
			if !next[id].Equal(simScheduleTime(m.GetNextDue())) {
				return fmt.Errorf("invariant: firing next_due %s/%s", sid, id)
			}
		}
		if m := e.GetScheduleSkipped(); m != nil {
			id := m.GetScheduleId()
			if next[id].IsZero() || !next[id].Equal(m.GetFirstDue().AsTime()) || m.GetCount() == 0 {
				return fmt.Errorf("invariant: skipped gap %s/%s", sid, id)
			}
			last := time.Time{}
			for n := uint32(0); n < m.GetCount(); n++ {
				last = next[id]
				if last.IsZero() || last.After(e.GetTime().AsTime()) {
					return fmt.Errorf("invariant: skipped future occurrence %s/%s", sid, id)
				}
				next[id], _ = plans[id].Next(last)
			}
			if !last.Equal(m.GetLastDue().AsTime()) || (closed != (m.GetReason() == "closed")) {
				return fmt.Errorf("invariant: skipped range %s/%s", sid, id)
			}
			if closed {
				next[id] = time.Time{}
			}
			if !next[id].Equal(simScheduleTime(m.GetNextDue())) {
				return fmt.Errorf("invariant: skipped next_due %s/%s", sid, id)
			}
		}
		if e.GetRunRequested() != nil {
			active = true
		}
		if e.GetRunCompleted() != nil || e.GetRunFailed() != nil || e.GetRunInterrupted() != nil {
			active = false
		}
		if e.GetSessionClosed() != nil {
			closed = true
		}
	}
	for id, due := range next {
		if !st.Schedules[id].NextDue.Equal(due) {
			return fmt.Errorf("invariant: schedule projection %s/%s", sid, id)
		}
	}
	deleted, err := lifecycle.Deleted(context.Background(), w.deletions, sid)
	if err != nil {
		return err
	}
	if q, ok := w.queue.(*memqueue.Queue); ok && !deleted && st.Active() == nil && len(st.OpenSchedules()) > 0 && !q.Has(sid) {
		return fmt.Errorf("invariant: schedule %s left queue", sid)
	}
	return nil
}

// 关闭的 Session 也要到点记录 closed；删除后则不再要求日志存在。
func (w *World) checkSchedulesSettled() error {
	for _, sid := range w.known() {
		st, err := w.store.Load(context.Background(), sid)
		if err != nil {
			return err
		}
		deleted, err := lifecycle.Deleted(context.Background(), w.deletions, sid)
		if err != nil {
			return err
		}
		if !deleted {
			if due, ok := st.NextScheduleDue(); ok && !due.After(w.clock.Now()) {
				item := st.OpenSchedules()[0]
				active := "none"
				if r := st.Active(); r != nil {
					active = fmt.Sprintf("%s status=%s turns=%d", r.ID, r.Status, r.Turns)
				}
				queued := "unknown"
				if q, ok := w.queue.(*memqueue.Queue); ok {
					queued = fmt.Sprint(q.Has(sid))
				}
				return fmt.Errorf("liveness: schedule %s/%s still due: now=%s next=%s last=%s open=%d closed=%t queued=%s seq=%d active=%s; runs: %s",
					sid, item.Created.GetScheduleId(), w.clock.Now().Format(time.RFC3339), due.Format(time.RFC3339), item.Last.Format(time.RFC3339), len(st.OpenSchedules()), st.Closed != nil, queued, st.Seq, active, w.stuck())
			}
		}
	}
	return nil
}

func (w *World) countSchedules(events []*v1.Event) {
	for _, e := range events {
		if e.GetScheduleFired() != nil {
			w.Stats.SchedulesFired++
		}
		if e.GetScheduleSkipped() != nil {
			w.Stats.SchedulesSkipped++
		}
		if e.GetScheduleCancelled() != nil {
			w.Stats.SchedulesCancelled++
		}
	}
}

func (w *World) fingerprintSchedules(h hash.Hash, sid string) {
	st, err := w.store.Load(context.Background(), sid)
	if err != nil {
		return
	}
	for _, item := range st.OpenSchedules() {
		fmt.Fprintf(h, "schedule|%s|%s|%s|%s;", sid, item.Created.GetScheduleId(), item.Last.UTC().Format(time.RFC3339Nano), item.NextDue.UTC().Format(time.RFC3339Nano))
	}
}

func simScheduleTime(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

// scheduleRunTurns 只读最新 Run 的日志，避免模型上下文压缩后把已做的工作忘掉。
func (w *World) scheduleRunTurns(ctx context.Context) (int, bool, error) {
	sid := faults.SessionFrom(ctx)
	if sid == "" {
		return 0, false, nil
	}
	events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
	if err != nil {
		return 0, false, err
	}
	turns := 0
	for i := len(events) - 1; i >= 0; i-- {
		if m := events[i].GetRunRequested(); m != nil {
			return turns, m.GetSchedule() != nil, nil
		}
		if events[i].GetAssistantMessage() != nil {
			turns++
		}
	}
	return 0, false, nil
}
