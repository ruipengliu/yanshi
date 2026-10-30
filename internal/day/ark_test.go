package day

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yanshi/internal/eventlog"
)

type arkObservation struct {
	Processed     bool   `json:"processed"`
	Filed         bool   `json:"filed"`
	Exchanges     int    `json:"exchanges"`
	Commitments   int    `json:"commitments"`
	Revision      uint64 `json:"capture_revision"`
	LiveCaptures  int    `json:"live_captures"`
	CompletedRuns int    `json:"completed_runs"`
	FailedRuns    int    `json:"failed_runs"`
	Inbox         int    `json:"inbox"`
	Sent          int    `json:"sent"`
}
type arkReport struct {
	Version     int                                `json:"version"`
	Stages      map[string]arkObservation          `json:"stages"`
	Outcomes    map[string]bool                    `json:"outcomes"`
	Baseline    string                             `json:"baseline"`
	Differences []string                           `json:"differences"`
	Diagnostics map[string]arkExecutionObservation `json:"diagnostics,omitempty"`
}

// Ark 共用三库、角色、时钟、采集正文与阶段顺序；模型结果只记报告，不决定脚本验收是否正确。
// 只有构建/配置/HTTP/PG 失败使该入口失败，缺少领域事实记为 outcome=false。
func TestDayArk(t *testing.T) {
	if os.Getenv("YANSHI_DAY_ARK") != "1" {
		t.Skip("use make day-ark")
	}
	e := newEnvironment(t, true)
	report := arkReport{Version: 1, Stages: map[string]arkObservation{}, Outcomes: map[string]bool{}, Baseline: "not_recorded", Differences: []string{}}
	tracker := new(arkExecutionTracker)
	report.Diagnostics = map[string]arkExecutionObservation{}
	execution := func(stage string) { report.Diagnostics[stage] = e.arkExecution(tracker) }
	observe := func(stage string) {
		d := e.day(businessDay)
		state := e.delivery()
		sent := 0
		for _, job := range state.Jobs {
			if job.State == "sent" {
				sent++
			}
		}
		report.Stages[stage] = arkObservation{Processed: d.Processed, Filed: len(d.Summary) > 0 && string(d.Summary) != "null", Exchanges: len(d.Exchanges),
			Commitments: len(e.commitments()), Revision: d.Revision, LiveCaptures: e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`),
			CompletedRuns: e.count(e.kernelPool, `SELECT count(*) FROM outbox WHERE kind='run_completed'`),
			FailedRuns:    e.count(e.kernelPool, `SELECT count(*) FROM outbox WHERE kind='run_failed'`), Inbox: len(state.Items), Sent: sent}
		execution(stage)
		t.Log("ark phase=" + stage)
	}
	wait := func(fn func() bool) bool {
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			if fn() {
				return true
			}
			time.Sleep(200 * time.Millisecond)
		}
		return false
	}
	organized := func(date string, revision uint64) bool {
		return wait(func() bool {
			var d dayView
			if !e.readBFF("/v1/days/"+date, &d) {
				return false
			}
			return e.dayFiled(date, revision, d)
		})
	}
	terminal := func(sid, rid string) bool {
		return wait(func() bool {
			events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
			must(t, err)
			for _, event := range events {
				if event.GetRunCompleted().GetRunId() == rid || event.GetRunFailed().GetRunId() == rid {
					return true
				}
			}
			return false
		})
	}
	// 报告即使某一步超时仍写入；不把第一份未经实测的数据伪称 Ark 基线。
	defer func() {
		baseline := os.Getenv("YANSHI_DAY_BASELINE")
		if baseline != "" {
			if !filepath.IsAbs(baseline) {
				baseline = filepath.Join(e.root, baseline)
			}
			raw, err := os.ReadFile(baseline)
			must(t, err)
			var previous arkReport
			must(t, json.Unmarshal(raw, &previous))
			if previous.Version != report.Version {
				t.Fatal("incompatible Ark day baseline")
			}
			report.Baseline = baseline
			report.Differences = arkDifferences(previous, report)
		}
		path := os.Getenv("YANSHI_DAY_REPORT")
		if path == "" {
			path = filepath.Join(e.root, "eval-results", "day-ark.json")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.root, path)
		}
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		raw, err := json.MarshalIndent(report, "", "  ")
		must(t, err)
		must(t, os.WriteFile(path, append(raw, '\n'), 0600))
		t.Logf("Ark day report=%s baseline=%s differences=%d", path, report.Baseline, len(report.Differences))
	}()

	e.enqueue(businessDay, "张总，办公室装修的报价单我明天发给你。", "08:30:00")
	second := e.enqueue(businessDay, "李总，部门预算表我明天发给你。", "08:50:00")
	e.bff.offline.Store(true)
	e.flush("unavailable")
	e.bff.offline.Store(false)
	e.bff.lose.Store(true)
	e.flush("unavailable")
	e.flush("")
	observe("morning-upload")
	e.start("orchestrator")
	e.advance("2035-01-08T09:10:00+08:00")
	report.Outcomes["daytime_review"] = organized(businessDay, 2)
	observe("daytime-review")
	e.stop("orchestrator")
	e.advance("2035-01-08T14:00:00+08:00")
	e.enqueue(businessDay, "张总，办公室装修的报价单改到后天发给你。", "14:00:00")
	e.flush("")
	e.advance("2035-01-08T14:10:00+08:00")
	e.start("orchestrator")
	report.Outcomes["afternoon_review"] = organized(businessDay, 3)
	execution("afternoon-review")
	for _, c := range e.commitments() {
		if c.Due == "2035-01-10" {
			report.Outcomes["model_rescheduled"] = true
		}
		if !report.Outcomes["user_edit"] {
			e.bffRequest("PATCH", "/v1/commitments/"+c.ID, map[string]any{"rev": c.Rev, "fields": map[string]string{"due_date": "2035-01-12"}}, 200, nil)
			report.Outcomes["user_edit"] = true
		}
	}
	observe("user-edit")
	e.stop("orchestrator")
	e.advance("2035-01-08T23:58:00+08:00")
	e.enqueue(businessDay, "闲聊：今天辛苦了。", "23:58:00")
	e.flush("")
	e.advance("2035-01-09T00:01:00+08:00")
	e.enqueue("2035-01-09", "闲聊：明早见。", "00:01:00")
	e.flush("")
	e.advance("2035-01-09T04:31:00+08:00")
	e.start("orchestrator")
	report.Outcomes["final_review"] = organized(businessDay, 4)
	observe("midnight-final")
	sid := e.newThread()
	rid := e.input(sid, "查询我的承诺，告诉我报价单和预算表分别安排在哪天。")
	report.Outcomes["assistant_terminal"] = terminal(sid, rid)
	var redo struct {
		ID string `json:"run_id"`
	}
	if report.Outcomes["assistant_terminal"] {
		e.ensureAuth()
		code, raw := request(e.ctx, e.bff.server.URL+"/v1/threads/"+sid+"/redo", "POST", e.token, map[string]string{"input_id": randomID(t), "expected_run_id": rid})
		if code != 202 && code != 409 {
			t.Fatalf("Ark redo HTTP status %d", code)
		}
		report.Outcomes["redo_eligible"] = code == 202
		if code == 202 {
			must(t, json.Unmarshal(raw, &redo))
			report.Outcomes["redo_terminal"] = terminal(sid, redo.ID)
		}
	}
	due := e.morningSchedule(sid, "查询并汇报当前未完成的承诺。")
	e.advance(due.Add(time.Minute).Format(time.RFC3339))
	report.Outcomes["scheduled_push"] = wait(func() bool {
		events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
		must(t, err)
		for _, event := range events {
			if r := event.GetRunRequested(); r.GetSchedule().GetScheduleId() == "morning-report" {
				return e.control.sent(r.GetRunId(), "run_completed") > 0
			}
		}
		return false
	})
	observe("assistant-scheduled-inbox")
	report.Outcomes["settled_before_restart"] = wait(e.settled)
	for _, role := range []string{"persona", "bff", "orchestrator"} {
		e.start(role)
	}
	observe("roles-restarted")
	e.bffRequest("POST", "/v1/days/"+businessDay+"/erase", map[string]string{"from": "#2.0", "to": "#2.0"}, 202, nil)
	code, _ := request(e.ctx, e.bff.server.URL+"/v1/captures/batches", "POST", e.token, second)
	report.Outcomes["erased_upload_rejected"] = code == 409 || code == 410
	report.Outcomes["erasure_complete"] = wait(func() bool {
		return e.count(e.personaPool, `SELECT count(*) FROM deletion_tasks WHERE status='pending'`) == 0
	})
	report.Outcomes["erasure_reprocessed"] = organized(businessDay, e.day(businessDay).Revision)
	observe("erasure")
	ledger := len(e.commitments())
	e.advance("2035-01-18T09:00:00+08:00")
	report.Outcomes["text_expired"] = wait(func() bool { return e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`) == 0 })
	report.Outcomes["ledger_retained"] = report.Outcomes["erasure_reprocessed"] && len(e.commitments()) == ledger
	observe("natural-expiry")
}
