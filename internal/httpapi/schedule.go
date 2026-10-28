package httpapi

import (
	"net/http"
	"time"

	"yanshi/internal/model"
	"yanshi/internal/schedule"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

type scheduleView struct {
	ScheduleID   string     `json:"schedule_id"`
	Input        string     `json:"input"`
	At           string     `json:"at,omitempty"`
	RRule        string     `json:"rrule,omitempty"`
	TimeZone     string     `json:"time_zone"`
	Until        *time.Time `json:"until,omitempty"`
	Start        time.Time  `json:"start"`
	NextDue      *time.Time `json:"next_due"`
	NextDueLocal string     `json:"next_due_local"`
	By           string     `json:"by"`
}

func (s *Server) scheduleView(item *session.Schedule) scheduleView {
	m := item.Created
	v := scheduleView{ScheduleID: m.GetScheduleId(), At: m.GetAt(), RRule: m.GetRrule(), TimeZone: m.GetTimeZone(), Start: m.GetStart().AsTime(), By: m.GetBy()}
	for _, b := range m.GetInput() {
		v.Input += b.GetText().GetText()
	}
	if m.GetUntil() != nil {
		t := m.GetUntil().AsTime()
		v.Until = &t
	}
	if !item.NextDue.IsZero() {
		t := item.NextDue
		v.NextDue = &t
		if loc, err := time.LoadLocation(m.GetTimeZone()); err == nil {
			v.NextDueLocal = schedule.FormatLocal(t, loc, s.Service.Store.Clock.Now())
		}
	}
	return v
}

func scheduleActor(r *http.Request) string {
	p := principal(r)
	if p.Unrestricted || p.Service() {
		return "service"
	}
	return "end_user"
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ScheduleID string    `json:"schedule_id"`
		Input      string    `json:"input"`
		At         string    `json:"at"`
		RRule      string    `json:"rrule"`
		TimeZone   string    `json:"time_zone"`
		Until      time.Time `json:"until"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	item, err := s.Service.CreateSchedule(owned(r), r.PathValue("id"), service.CreateScheduleRequest{ScheduleID: req.ScheduleID, Input: model.TextBlocks(req.Input), At: req.At, RRule: req.RRule, TimeZone: req.TimeZone, Until: req.Until, By: scheduleActor(r)})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.scheduleView(item))
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	items, err := s.Service.ListSchedules(owned(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []scheduleView{}
	for _, item := range items {
		out = append(out, s.scheduleView(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (s *Server) cancelSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.Service.CancelSchedule(owned(r), r.PathValue("id"), r.PathValue("schedule_id"), scheduleActor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
