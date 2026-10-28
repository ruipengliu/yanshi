package session

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/schedule"
)

const MaxSchedules = 50

// Schedule 保留声明与处理进度；失效后保留声明使创建重试不会复活定时。
type Schedule struct {
	Created *v1.ScheduleCreated
	Last    time.Time
	NextDue time.Time
}

func (s *Schedule) Plan() (*schedule.Plan, error) {
	m := s.Created
	return schedule.Compile(schedule.Spec{At: m.GetAt(), RRule: m.GetRrule(), TimeZone: m.GetTimeZone(), Until: fromTS(m.GetUntil())}, fromTS(m.GetStart()))
}

// ScheduleInputID 不依赖 Worker 或重试次数，且不允许定时 ID 中的分隔符造成碰撞。
func ScheduleInputID(id string, due time.Time) string {
	return fmt.Sprintf("schedule_%x", sha256.Sum256([]byte(id+"\x00"+due.UTC().Format(time.RFC3339Nano))))
}

func ScheduleText(input []*v1.ContentBlock) bool {
	if len(input) == 0 {
		return false
	}
	var text strings.Builder
	for _, b := range input {
		if b.GetText() == nil {
			return false
		}
		text.WriteString(b.GetText().GetText())
	}
	return strings.TrimSpace(text.String()) != ""
}

func ScheduleBy(by string) bool { return by == "end_user" || by == "service" || by == "agent" }

// OpenSchedules 按到期、ID 排序，使同刻到期的处理顺序不依赖 map 遍历。
func (s *State) OpenSchedules() []*Schedule {
	out := []*Schedule{}
	for _, item := range s.Schedules {
		if !item.NextDue.IsZero() {
			out = append(out, item)
		}
	}
	slices.SortFunc(out, func(a, b *Schedule) int {
		if c := a.NextDue.Compare(b.NextDue); c != 0 {
			return c
		}
		return strings.Compare(a.Created.GetScheduleId(), b.Created.GetScheduleId())
	})
	return out
}

func (s *State) NextScheduleDue() (time.Time, bool) {
	items := s.OpenSchedules()
	if len(items) == 0 {
		return time.Time{}, false
	}
	return items[0].NextDue, true
}

func (s *State) applySchedule(e *v1.Event) error {
	if m := e.GetScheduleCreated(); m != nil {
		if m.GetScheduleId() == "" || len(m.GetScheduleId()) > 128 || strings.ContainsAny(m.GetScheduleId(), "\r\n\x00") || s.Schedules[m.GetScheduleId()] != nil || len(s.OpenSchedules()) >= MaxSchedules || s.Created.GetParent() != nil || !ScheduleText(m.GetInput()) || !ScheduleBy(m.GetBy()) {
			return fmt.Errorf("invalid schedule declaration")
		}
		if m.GetStart() == nil || m.GetStart().CheckValid() != nil || m.GetStart().AsTime().After(e.GetTime().AsTime()) || m.GetNextDue() == nil || m.GetNextDue().CheckValid() != nil || (m.GetUntil() != nil && m.GetUntil().CheckValid() != nil) {
			return fmt.Errorf("invalid schedule timestamps")
		}
		// UTC 是日志事实；规则解析只在写入方进行，不能因 tzdata 升级拒绝旧日志。
		if (m.GetAt() == "") == (m.GetRrule() == "") || m.GetTimeZone() == "" || !m.GetNextDue().AsTime().After(m.GetStart().AsTime()) {
			return fmt.Errorf("invalid schedule structure")
		}
		item := &Schedule{Created: m, Last: fromTS(m.GetStart()), NextDue: fromTS(m.GetNextDue())}
		if s.Schedules == nil {
			s.Schedules = map[string]*Schedule{}
		}
		s.Schedules[m.GetScheduleId()] = item
		return nil
	}
	id := ""
	switch {
	case e.GetScheduleCancelled() != nil:
		id = e.GetScheduleCancelled().GetScheduleId()
	case e.GetScheduleFired() != nil:
		id = e.GetScheduleFired().GetScheduleId()
	case e.GetScheduleSkipped() != nil:
		id = e.GetScheduleSkipped().GetScheduleId()
	}
	old := s.Schedules[id]
	if old == nil || old.NextDue.IsZero() {
		return fmt.Errorf("schedule is absent or inactive")
	}
	item := *old
	if m := e.GetScheduleCancelled(); m != nil {
		if !ScheduleBy(m.GetBy()) {
			return fmt.Errorf("invalid schedule cancellation by")
		}
		item.NextDue = time.Time{}
	} else if m := e.GetScheduleFired(); m != nil {
		due, next := fromTS(m.GetDueAt()), fromTS(m.GetNextDue())
		if m.GetDueAt() == nil || m.GetDueAt().CheckValid() != nil || (m.GetNextDue() != nil && m.GetNextDue().CheckValid() != nil) || (m.GetNextDue() != nil && !next.After(due)) || s.Active() != nil || m.GetRunId() == "" || !due.Equal(old.NextDue) || due.After(e.GetTime().AsTime()) {
			return fmt.Errorf("invalid schedule firing")
		}
		item.Last, item.NextDue = due, next
	} else if m := e.GetScheduleSkipped(); m != nil {
		if m.GetFirstDue() == nil || m.GetLastDue() == nil || m.GetFirstDue().CheckValid() != nil || m.GetLastDue().CheckValid() != nil || (m.GetNextDue() != nil && m.GetNextDue().CheckValid() != nil) {
			return fmt.Errorf("invalid skipped timestamps")
		}
		first, last, next := fromTS(m.GetFirstDue()), fromTS(m.GetLastDue()), fromTS(m.GetNextDue())
		if !first.Equal(old.NextDue) || last.Before(first) || m.GetCount() == 0 || last.After(e.GetTime().AsTime()) || (m.GetNextDue() != nil && !next.After(last)) {
			return fmt.Errorf("invalid skipped schedule range")
		}
		switch m.GetReason() {
		case "missed":
			if s.Closed != nil || s.Active() != nil {
				return fmt.Errorf("invalid missed schedule")
			}
		case "closed":
			if s.Closed == nil || m.GetNextDue() != nil {
				return fmt.Errorf("invalid closed schedule")
			}
		default:
			return fmt.Errorf("invalid skip reason")
		}
		item.Last, item.NextDue = last, next
	}
	s.Schedules[id] = &item
	if e.GetScheduleFired() != nil {
		s.pendingSchedule = e
	}
	return nil
}

func (s *State) checkScheduleInput(e *v1.Event) error {
	m := e.GetRunRequested()
	mark := m.GetSchedule()
	f := s.pendingSchedule.GetScheduleFired()
	if mark == nil && f == nil {
		if strings.HasPrefix(m.GetInputId(), "schedule_") {
			return fmt.Errorf("reserved schedule input_id")
		}
		return nil
	}
	if mark == nil || f == nil || s.pendingSchedule.GetSeq()+1 != e.GetSeq() || !proto.Equal(s.pendingSchedule.GetTime(), e.GetTime()) || mark.GetScheduleId() != f.GetScheduleId() || !proto.Equal(mark.GetDueAt(), f.GetDueAt()) || m.GetRunId() != f.GetRunId() || m.GetFromCall() != "" || m.GetInputId() != ScheduleInputID(mark.GetScheduleId(), fromTS(mark.GetDueAt())) || !ScheduleText(m.GetInput()) {
		return fmt.Errorf("unpaired schedule input")
	}
	item := s.Schedules[mark.GetScheduleId()]
	if !proto.Equal(&v1.RunRequested{Input: m.GetInput()}, &v1.RunRequested{Input: item.Created.GetInput()}) {
		return fmt.Errorf("schedule input differs from declaration")
	}
	return nil
}
