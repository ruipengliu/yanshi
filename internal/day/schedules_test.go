package day

import (
	"testing"
	"time"

	"yanshi/internal/eventlog"
	"yanshi/internal/model"
)

const morningScheduleID = "morning-report"
const morningScheduleAt = "2035-01-09T09:00:00"
const morningScheduleZone = "Asia/Shanghai"

type scheduleView struct {
	ID       string    `json:"schedule_id"`
	Input    string    `json:"input"`
	At       string    `json:"at"`
	RRule    string    `json:"rrule"`
	TimeZone string    `json:"time_zone"`
	NextDue  time.Time `json:"next_due"`
}

func (e *environment) morningSchedule(sid, input string) time.Time {
	e.t.Helper()
	zone, err := time.LoadLocation(morningScheduleZone)
	must(e.t, err)
	// at 是当地钟点，拨钟参数才是绝对时间；不能把 RFC3339 原样交给定时接口。
	due, err := time.ParseInLocation("2006-01-02T15:04:05", morningScheduleAt, zone)
	must(e.t, err)
	if !due.After(e.control.Now()) {
		e.t.Fatal("scenario reached scheduled time before creation")
	}
	var created scheduleView
	path := "/v1/threads/" + sid + "/schedules"
	e.bffRequest("POST", path, map[string]string{"schedule_id": morningScheduleID, "input": input, "at": morningScheduleAt, "time_zone": morningScheduleZone}, 201, &created)
	if created.ID != morningScheduleID || created.Input != input || created.At != morningScheduleAt || created.RRule != "" || created.TimeZone != morningScheduleZone || !created.NextDue.Equal(due) {
		e.t.Fatal("schedule declaration or resolved UTC time changed")
	}
	var listed struct {
		Schedules []scheduleView `json:"schedules"`
	}
	e.bffRequest("GET", path, nil, 200, &listed)
	if len(listed.Schedules) != 1 || listed.Schedules[0] != created {
		e.t.Fatal("created schedule is not visible through BFF")
	}
	events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
	must(e.t, err)
	count := 0
	for _, event := range events {
		if item := event.GetScheduleCreated(); item.GetScheduleId() == morningScheduleID {
			count++
			if item.GetAt() != morningScheduleAt || item.GetTimeZone() != morningScheduleZone || !item.GetNextDue().AsTime().Equal(due) || model.Text(item.GetInput()) != input {
				e.t.Fatal("kernel schedule Event differs from BFF declaration")
			}
		}
	}
	if count != 1 {
		e.t.Fatal("schedule creation Event missing or duplicated")
	}
	return due
}

func (e *environment) scheduledRun(sid string, due time.Time) string {
	e.t.Helper()
	var run string
	eventually(e.t, "scheduled Run and fire Events", func() bool {
		events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
		must(e.t, err)
		requested, fired := 0, 0
		var firedRun string
		for _, event := range events {
			if r := event.GetRunRequested(); r.GetSchedule().GetScheduleId() == morningScheduleID {
				requested++
				run = r.GetRunId()
				if !r.GetSchedule().GetDueAt().AsTime().Equal(due) {
					e.t.Fatal("scheduled Run has the wrong due time")
				}
			}
			if item := event.GetScheduleFired(); item.GetScheduleId() == morningScheduleID {
				fired++
				firedRun = item.GetRunId()
				if !item.GetDueAt().AsTime().Equal(due) || item.GetNextDue() != nil {
					e.t.Fatal("one-shot schedule fired with the wrong due time or remained active")
				}
			}
		}
		if requested > 1 || fired > 1 {
			e.t.Fatal("one-shot schedule fired more than once")
		}
		return requested == 1 && fired == 1 && run != "" && run == firedRun
	})
	return run
}

func TestDayScheduleHTTP(t *testing.T) {
	e := newEnvironment(t, false)
	sid := e.newThread()
	path := "/v1/threads/" + sid + "/schedules"
	// 同一条 BFF→内核 HTTP 链路必须拒绝带偏移的 at、过去的钟点和缺钟点的 RRULE。
	for _, rule := range []map[string]any{
		{"at": "2035-01-09T09:00:00+08:00", "input": "hello"},
		{"at": "2035-01-09T01:00:00Z", "input": "hello"},
		{"at": "2035-01-08T08:59:00", "input": "hello"},
		{"rrule": "FREQ=DAILY", "input": "hello"},
		{"at": morningScheduleAt, "input": []string{"hello"}},
	} {
		rule["time_zone"] = morningScheduleZone
		e.bffRequest("POST", path, rule, 400, nil)
	}
	var rejected struct {
		Schedules []scheduleView `json:"schedules"`
	}
	e.bffRequest("GET", path, nil, 200, &rejected)
	if len(rejected.Schedules) != 0 {
		t.Fatal("invalid schedule left an active declaration")
	}
	var recurring struct {
		scheduleView
		Until time.Time `json:"until"`
	}
	e.bffRequest("POST", path, map[string]string{"schedule_id": "daily-report", "input": "hello", "rrule": "FREQ=DAILY;BYHOUR=9;BYMINUTE=0", "time_zone": morningScheduleZone, "until": "2035-01-10T01:00:00Z"}, 201, &recurring)
	if recurring.ID != "daily-report" || recurring.Input != "hello" || recurring.At != "" || recurring.RRule == "" || recurring.TimeZone != morningScheduleZone || !recurring.NextDue.Equal(time.Date(2035, 1, 9, 1, 0, 0, 0, time.UTC)) || !recurring.Until.Equal(time.Date(2035, 1, 10, 1, 0, 0, 0, time.UTC)) {
		t.Fatal("recurring schedule or timestamp until did not match the kernel contract")
	}
	e.bffRequest("DELETE", path+"/daily-report", nil, 204, nil)
	due := e.morningSchedule(sid, "call mcp_xinli__list_commitments {}")
	e.start("orchestrator")
	e.advance(due.Add(time.Minute).Format(time.RFC3339))
	run := e.scheduledRun(sid, due)
	e.run(sid, run, "completed")
	e.waitSent(run)
	e.bffRequest("GET", path, nil, 200, &rejected)
	if len(rejected.Schedules) != 0 {
		t.Fatal("one-shot schedule remained active after firing")
	}
	e.settle("schedule-http")
}
