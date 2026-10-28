package service

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/schedule"
	"yanshi/internal/session"
)

type CreateScheduleRequest struct {
	ScheduleID string
	Input      []*v1.ContentBlock
	At         string
	RRule      string
	TimeZone   string
	Until      time.Time
	By         string
}

func scheduleTS(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func (s *Service) CreateSchedule(ctx context.Context, sessionID string, req CreateScheduleRequest) (*session.Schedule, error) {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if st.Closed != nil {
		return nil, fmt.Errorf("%w: session is closed", ErrConflict)
	}
	if st.Created.GetParent() != nil {
		return nil, fmt.Errorf("%w: delegated sessions cannot create schedules", ErrInvalid)
	}
	if req.TimeZone == "" {
		req.TimeZone = st.Created.GetTimeZone()
	}
	if req.TimeZone == "" {
		return nil, fmt.Errorf("%w: 定时与 Session 均未设置 time_zone，请显式填写 IANA 时区", ErrInvalid)
	}
	if err := rejectPlatformOnly(req.Input); err != nil {
		return nil, err
	}
	if !session.ScheduleText(req.Input) || !session.ScheduleBy(req.By) {
		return nil, fmt.Errorf("%w: schedule requires text input and a valid creator", ErrInvalid)
	}
	if err := s.moderateInput(ctx, st, req.Input); err != nil {
		return nil, err
	}
	if req.ScheduleID == "" {
		req.ScheduleID = "sched_" + s.Store.IDs()
	}
	if len(req.ScheduleID) > 128 || strings.ContainsAny(req.ScheduleID, "\r\n\x00") {
		return nil, fmt.Errorf("%w: invalid schedule_id", ErrInvalid)
	}
	if !req.Until.IsZero() && timestamppb.New(req.Until).CheckValid() != nil {
		return nil, fmt.Errorf("%w: invalid until", ErrInvalid)
	}
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		if st.Closed != nil {
			return session.Decision{}, fmt.Errorf("%w: session is closed", ErrConflict)
		}
		if st.Created.GetParent() != nil {
			return session.Decision{}, fmt.Errorf("%w: delegated sessions cannot create schedules", ErrInvalid)
		}
		start := s.Store.Clock.Now()
		old := st.Schedules[req.ScheduleID]
		if old != nil {
			start = old.Created.GetStart().AsTime()
			// 规范化声明完全相同时无需重新解析；已登记的时区即使退役，重试仍幂等。
			candidate := &v1.ScheduleCreated{ScheduleId: req.ScheduleID, Input: req.Input, TimeZone: req.TimeZone, At: req.At, Rrule: req.RRule, Until: scheduleTS(req.Until), Start: old.Created.GetStart(), NextDue: old.Created.GetNextDue(), By: req.By}
			if proto.Equal(candidate, old.Created) {
				return session.Decision{}, nil
			}
		}
		p, err := schedule.Compile(schedule.Spec{At: req.At, RRule: req.RRule, TimeZone: req.TimeZone, Until: req.Until}, start)
		if err != nil {
			if old != nil {
				return session.Decision{}, fmt.Errorf("%w: schedule_id already has another declaration", ErrConflict)
			}
			return session.Decision{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		spec := p.Spec()
		next, _ := p.Next(start)
		m := &v1.ScheduleCreated{ScheduleId: req.ScheduleID, Input: req.Input, TimeZone: spec.TimeZone, At: spec.At, Rrule: spec.RRule, Until: scheduleTS(spec.Until), Start: scheduleTS(start), NextDue: scheduleTS(next), By: req.By}
		if old != nil {
			// 已保存的首次 UTC 不因当前 tzdata 改变而使重试冲突。
			m.NextDue = old.Created.GetNextDue()
			if !proto.Equal(old.Created, m) {
				return session.Decision{}, fmt.Errorf("%w: schedule_id already has another declaration", ErrConflict)
			}
			return session.Decision{}, nil
		}
		if len(st.OpenSchedules()) >= session.MaxSchedules {
			return session.Decision{}, fmt.Errorf("%w: at most %d active schedules", ErrInvalid, session.MaxSchedules)
		}
		return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}}, Wake: true}, nil
	})
	if err != nil {
		return nil, gone(sessionID, err)
	}
	if err := s.SyncScheduleIndex(ctx, st); err != nil {
		return nil, err
	}
	return st.Schedules[req.ScheduleID], nil
}

func (s *Service) ListSchedules(ctx context.Context, sessionID string) ([]*session.Schedule, error) {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return st.OpenSchedules(), nil
}

func (s *Service) CancelSchedule(ctx context.Context, sessionID, scheduleID, by string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	if !session.ScheduleBy(by) {
		return fmt.Errorf("%w: invalid schedule cancellation by", ErrInvalid)
	}
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		item := st.Schedules[scheduleID]
		if item == nil || item.NextDue.IsZero() || st.Closed != nil {
			return session.Decision{}, fmt.Errorf("%w: active schedule %s", ErrNotFound, scheduleID)
		}
		return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_ScheduleCancelled{ScheduleCancelled: &v1.ScheduleCancelled{ScheduleId: scheduleID, By: by}}}}, Wake: true}, nil
	})
	if err != nil {
		return gone(sessionID, err)
	}
	return s.SyncScheduleIndex(ctx, st)
}

// FireSchedule 只在无活跃（含排队）Run 时推进一次。冲突后重新计算，配额不参与到期决策。
// Worker 已持有租约，因此不 Wake；追加失败或成功后崩溃均由同一日志进度恢复。
func (s *Service) FireSchedule(ctx context.Context, st *session.State, scheduleID string) error {
	var prev time.Time
	fired := false
	err := s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		fired = false
		item := st.Schedules[scheduleID]
		now := s.Store.Clock.Now()
		if item == nil || item.NextDue.IsZero() || item.NextDue.After(now) || st.Active() != nil {
			return session.Decision{}, nil
		}
		c, err := scheduleCatchup(item, now)
		if err != nil {
			return session.Decision{}, err
		}
		var events []*v1.Event
		if st.Closed != nil {
			first := c.Fire
			if c.Skipped.Count > 0 {
				first = c.Skipped.First
			}
			if c.Skipped.Count >= math.MaxUint32 {
				return session.Decision{}, fmt.Errorf("schedule skipped count overflows uint32")
			}
			return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_ScheduleSkipped{ScheduleSkipped: &v1.ScheduleSkipped{ScheduleId: scheduleID, FirstDue: scheduleTS(first), LastDue: scheduleTS(c.Fire), Count: uint32(c.Skipped.Count + 1), Reason: "closed"}}}}}, nil
		}
		if c.Skipped.Count > 0 {
			if c.Skipped.Count > math.MaxUint32 {
				return session.Decision{}, fmt.Errorf("schedule skipped count overflows uint32")
			}
			events = append(events, &v1.Event{Payload: &v1.Event_ScheduleSkipped{ScheduleSkipped: &v1.ScheduleSkipped{ScheduleId: scheduleID, FirstDue: scheduleTS(c.Skipped.First), LastDue: scheduleTS(c.Skipped.Last), Count: uint32(c.Skipped.Count), Reason: "missed", NextDue: scheduleTS(c.Fire)}}})
		}
		if to, ok := s.Agents.Withdrawn(st.Agent); ok {
			events = append(events, &v1.Event{Payload: &v1.Event_AgentSwitched{AgentSwitched: &v1.AgentSwitched{From: st.Agent, To: to.Ref(), Reason: "withdrawn"}}})
		}
		runID := "run_" + s.Store.IDs()
		events = append(events, &v1.Event{Payload: &v1.Event_ScheduleFired{ScheduleFired: &v1.ScheduleFired{ScheduleId: scheduleID, DueAt: scheduleTS(c.Fire), NextDue: scheduleTS(c.Next), RunId: runID}}}, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: runID, Input: item.Created.GetInput(), InputId: session.ScheduleInputID(scheduleID, c.Fire), Schedule: &v1.ScheduleMark{ScheduleId: scheduleID, DueAt: scheduleTS(c.Fire)}}}})
		fired, prev = true, lastRunAt(st)
		return session.Decision{Events: events}, nil
	})
	if err != nil {
		return err
	}
	if err := s.SyncScheduleIndex(ctx, st); err != nil {
		return err
	}
	if !fired {
		return nil
	}
	return s.touch(ctx, st.SessionID, prev)
}

// scheduleCatchup 以已写入日志的 NextDue 为第一点，当前规则仅计算它之后的将来到期。
// 一次性定时无需重新解析当地时间，否则 tzdata 前移/后移可能遗漏或重复原定到期。
func scheduleCatchup(item *session.Schedule, now time.Time) (schedule.Catchup, error) {
	if item.Created.GetAt() != "" {
		return schedule.Catchup{Fire: item.NextDue}, nil
	}
	p, err := item.Plan()
	if err != nil {
		return schedule.Catchup{}, err
	}
	c := p.Catchup(item.NextDue, now)
	if c.Fire.IsZero() {
		c.Fire = item.NextDue
		return c, nil
	}
	// 当前规则在已保存的第一点之后还产生了到期，第一点也计入被合并的错过。
	if c.Skipped.Count == 0 {
		c.Skipped.Last = item.NextDue
	}
	c.Skipped.First = item.NextDue
	c.Skipped.Count++
	return c, nil
}

// SyncScheduleIndex 在日志之后维护可重建索引；乱序写入与崩溃遗漏由 Janitor 按投影校准。
func (s *Service) SyncScheduleIndex(ctx context.Context, st *session.State) error {
	if s.Index == nil {
		return nil
	}
	next, _ := st.NextScheduleDue()
	return s.Index.SetNextDue(ctx, st.SessionID, next)
}
