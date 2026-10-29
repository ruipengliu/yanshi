package day

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/outbox"
)

type batch struct {
	SessionID  string    `json:"session_id"`
	Seq        uint64    `json:"seq"`
	CaptureID  string    `json:"capture_id"`
	Day        string    `json:"business_day"`
	TimeZone   string    `json:"time_zone"`
	Generation uint64    `json:"generation"`
	Segments   []segment `json:"segments"`
}
type segment struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
	At      string `json:"at"`
}
type receipt struct {
	Status string   `json:"status"`
	Refs   []string `json:"refs"`
	Next   uint64   `json:"next_seq"`
}
type senderReply struct {
	Error string `json:"error"`
	Batch batch  `json:"batch"`
	State struct {
		Stopped string `json:"stopped"`
		Entries []struct {
			Batch   batch   `json:"batch"`
			Status  string  `json:"status"`
			Receipt receipt `json:"receipt"`
		} `json:"entries"`
	} `json:"state"`
}
type dayView struct {
	Date      string            `json:"date"`
	Progress  string            `json:"processing_position"`
	Processed bool              `json:"processed"`
	Revision  uint64            `json:"capture_revision"`
	Expired   bool              `json:"expired"`
	Expires   time.Time         `json:"expires_at"`
	Exchanges []json.RawMessage `json:"exchanges"`
	Summary   json.RawMessage   `json:"summary"`
}
type commitment struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Due     string `json:"due_date"`
	Rev     int    `json:"rev"`
}
type runView struct {
	ID      string `json:"run_id"`
	Status  string `json:"status"`
	Waiting *struct {
		Kind string `json:"kind"`
		Call string `json:"call_id"`
	} `json:"waiting"`
}

func (e *environment) sender(op, day, text, at string) senderReply {
	e.t.Helper()
	e.ensureAuth()
	in := map[string]any{"path": e.senderPath, "identity": map[string]string{"business_line": businessLine, "end_user": e.endUser, "device_id": "phone", "session_id": "capture-day"},
		"operation": op, "url": e.bff.server.URL, "token": e.token, "day": day, "generation": 0}
	if op == "enqueue" {
		in["segments"] = []segment{{"wearer", text, at}}
	}
	raw, err := json.Marshal(in)
	must(e.t, err)
	cmd := exec.CommandContext(e.ctx, e.bin, "day-sender")
	cmd.Stdin, cmd.Stderr = bytes.NewReader(raw), io.Discard
	output, err := cmd.Output()
	must(e.t, err)
	var out senderReply
	must(e.t, json.Unmarshal(output, &out))
	return out
}
func (e *environment) flush(want string) senderReply {
	e.t.Helper()
	out := e.sender("flush", "", "", "")
	if out.Error != want {
		e.t.Fatalf("sender error=%q want=%q", out.Error, want)
	}
	return out
}
func (e *environment) enqueue(day, text, clock string) batch {
	e.t.Helper()
	out := e.sender("enqueue", day, text, day+"T"+clock+"+08:00")
	if out.Error != "" {
		e.t.Fatal("sender enqueue failed")
	}
	return out.Batch
}
func (e *environment) day(date string) dayView {
	e.t.Helper()
	var d dayView
	e.bffRequest("GET", "/v1/days/"+date, nil, 200, &d)
	return d
}
func (e *environment) commitments() []commitment {
	var page struct {
		Items []commitment `json:"items"`
	}
	e.bffRequest("GET", "/v1/commitments?status=all", nil, 200, &page)
	return page.Items
}
func (e *environment) quote() commitment {
	for _, c := range e.commitments() {
		if strings.Contains(c.Content, "报价") {
			return c
		}
	}
	e.t.Fatal("quote commitment missing")
	return commitment{}
}
func (e *environment) advance(at string) {
	e.t.Helper()
	t, err := time.Parse(time.RFC3339, at)
	must(e.t, err)
	e.control.advance(e.t, t)
	for _, p := range e.processes {
		if p.running() {
			e.checkClock(p)
		}
	}
	must(e.t, e.janitor.Sweep(e.ctx))
}
func (e *environment) organized(date string, revision uint64) dayView {
	e.t.Helper()
	var d dayView
	eventually(e.t, "day processed and filed", func() bool {
		if p := e.processes["orchestrator"]; p != nil && !p.running() {
			e.t.Fatal("orchestrator exited before day was processed")
		}
		if !e.readBFF("/v1/days/"+date, &d) {
			return false
		}
		return e.dayFiled(date, revision, d)
	})
	return d
}

// 四层观察在每个阶段重复执行；输出只有计数，原文/工具参数不写验收日志。
func (e *environment) observe(label string) {
	e.t.Helper()
	// 三库没有共同读事务；等待相邻 HTTP/SQL 读取收敛，避免把正常后台提交误判为丢失。
	eventually(e.t, "four-layer observation: "+label, func() bool {
		d := e.day(businessDay)
		var stored struct {
			CaptureRevision uint64
			Progress        string
		}
		if !e.jsonRow(e.personaPool, `SELECT payload FROM business_days WHERE business_line=$1 AND end_user=$2 AND business_day=$3`, &stored, []byte(businessLine), []byte(e.endUser), []byte(businessDay)) {
			e.t.Fatal("Persona day missing")
		}
		if d.Revision != stored.CaptureRevision || d.Progress != stored.Progress {
			return false
		}
		commitments := e.commitments()
		if len(commitments) != e.count(e.personaPool, `SELECT count(*) FROM commitments WHERE business_line=$1 AND end_user=$2`, []byte(businessLine), []byte(e.endUser)) {
			return false
		}
		var storedCommitments []struct {
			ID        string            `json:"id"`
			Content   string            `json:"content"`
			Due       string            `json:"due_date"`
			Revisions []json.RawMessage `json:"revisions"`
		}
		e.jsonRow(e.personaPool, `SELECT coalesce(jsonb_agg(payload ORDER BY ordinal), '[]'::jsonb) FROM commitments WHERE business_line=$1 AND end_user=$2`, &storedCommitments, []byte(businessLine), []byte(e.endUser))
		for _, c := range commitments {
			found := false
			for _, stored := range storedCommitments {
				if c.ID == stored.ID {
					found = c.Content == stored.Content && c.Due == stored.Due && c.Rev == len(stored.Revisions)
				}
			}
			if !found {
				return false
			}
		}
		var storedExchanges []map[string]any
		e.jsonRow(e.personaPool, `SELECT coalesce(jsonb_agg(payload ORDER BY ordinal), '[]'::jsonb) FROM exchanges WHERE business_line=$1 AND end_user=$2 AND payload->>'date'=$3 AND coalesce(payload->>'merged_into','')=''`, &storedExchanges, []byte(businessLine), []byte(e.endUser), businessDay)
		if len(d.Exchanges) != len(storedExchanges) {
			return false
		}
		for _, raw := range d.Exchanges {
			var exchange map[string]any
			must(e.t, json.Unmarshal(raw, &exchange))
			delete(exchange, "rev")
			found := false
			for _, stored := range storedExchanges {
				if reflect.DeepEqual(exchange, stored) {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		var tasks struct {
			Items []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"items"`
		}
		e.bffRequest("GET", "/v1/tasks?limit=100", nil, 200, &tasks)
		if len(tasks.Items) != e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE business_line=$1 AND end_user=$2`, businessLine, e.endUser) {
			return false
		}
		var inbox struct {
			Items []json.RawMessage `json:"items"`
		}
		e.bffRequest("GET", "/v1/inbox?limit=100", nil, 200, &inbox)
		state := e.delivery()
		if len(inbox.Items) != len(state.Items) {
			return false
		}
		for _, raw := range inbox.Items {
			var item struct {
				ID string `json:"id"`
			}
			must(e.t, json.Unmarshal(raw, &item))
			if _, ok := state.Items[item.ID]; !ok {
				return false
			}
		}
		for id, job := range state.Jobs {
			if job.State == "sent" && e.control.sentID(id) == 0 {
				e.t.Fatal("sent job has no fake send observation")
			}
		}
		if !d.Expired {
			var captures struct {
				Revision uint64            `json:"capture_revision"`
				Items    []json.RawMessage `json:"items"`
			}
			e.bffRequest("GET", "/v1/days/"+businessDay+"/captures?limit=100", nil, 200, &captures)
			if captures.Revision != d.Revision || len(captures.Items) != e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE business_day=$1 AND live`, businessDay) {
				return false
			}
		}
		rows, err := e.core.Stores.Outbox.List(e.ctx, businessLine)
		must(e.t, err)
		logs := map[string][]*v1.Event{}
		for _, envelope := range rows {
			if envelope.Kind == outbox.SessionDeleted {
				continue
			}
			events, ok := logs[envelope.SessionID]
			if !ok {
				events, err = eventlog.ReadAll(e.ctx, e.core.Stores.Log, envelope.SessionID, 0)
				must(e.t, err)
				logs[envelope.SessionID] = events
			}
			found := false
			for _, event := range events {
				if event.GetSeq() != envelope.Seq {
					continue
				}
				found = true
				if envelope.Kind == outbox.RunCompleted && event.GetRunCompleted().GetRunId() != envelope.RunID {
					e.t.Fatal("completion envelope differs from Event")
				}
			}
			if !found {
				return false
			}
		}
		e.t.Logf("phase=%s revision=%d exchanges=%d commitments=%d tasks=%d inbox=%d envelopes=%d", label, d.Revision, len(d.Exchanges), len(commitments), len(tasks.Items), len(inbox.Items), len(rows))
		return true
	})
}

func (e *environment) morning() (batch, batch) {
	first := e.enqueue(businessDay, "张总，办公室装修的报价单我明天发给你。", "08:30:00")
	second := e.enqueue(businessDay, "李总，部门预算表我明天发给你。", "08:50:00")
	e.bff.offline.Store(true)
	e.flush("unavailable")
	if e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`) != 0 {
		e.t.Fatal("offline request was accepted")
	}
	e.bff.offline.Store(false)
	// 绕过正常顺序一次，证明缺口仅暂存，不分配正式引用。
	var staged receipt
	e.bffRequest("POST", "/v1/captures/batches", second, 202, &staged)
	if staged.Status != "staged" || len(staged.Refs) != 0 {
		e.t.Fatal("gap allocated references")
	}
	e.bff.lose.Store(true)
	e.flush("unavailable")
	if e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`) == 0 {
		e.t.Fatal("lost response did not follow commit")
	}
	// helper 是新进程，只从文件读取未确认批次。
	out := e.flush("")
	if len(out.State.Entries) != 2 {
		e.t.Fatal("sender lost entries across restart")
	}
	for i, b := range []batch{first, second} {
		var duplicate receipt
		e.bffRequest("POST", "/v1/captures/batches", b, 200, &duplicate)
		entry := out.State.Entries[i]
		if entry.Status != "accepted" || len(entry.Batch.Segments) != 0 || fmt.Sprint(duplicate.Refs) != fmt.Sprint(entry.Receipt.Refs) || len(duplicate.Refs) != 1 {
			e.t.Fatal("duplicate changed receipt or sender retained confirmed text")
		}
	}
	conflict := first
	conflict.Segments = []segment{{"wearer", "异文", first.Segments[0].At}}
	var failure struct {
		Code string `json:"code"`
	}
	e.bffRequest("POST", "/v1/captures/batches", conflict, 409, &failure)
	if failure.Code != "capture_conflict" {
		e.t.Fatal("same ID with different text was not a conflict")
	}
	before := e.day(businessDay)
	if before.Revision != 2 || before.Processed || len(before.Exchanges) != 0 {
		e.t.Fatal("upload advanced processing")
	}
	e.observe("morning-upload")
	e.start("orchestrator")
	e.advance("2035-01-08T09:10:00+08:00")
	e.organized(businessDay, 2)
	eventually(e.t, "two commitments and project fanout", func() bool {
		return len(e.commitments()) == 2 && e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE operation='collect' AND state='succeeded'`) >= 2
	})
	eventually(e.t, "project status materialized", func() bool {
		var page struct {
			Items []struct {
				Status    string    `json:"status"`
				Next      []string  `json:"next_steps"`
				Questions []string  `json:"open_questions"`
				At        time.Time `json:"status_at"`
				Session   string    `json:"session"`
			} `json:"items"`
		}
		e.bffRequest("GET", "/v1/projects?limit=100", nil, 200, &page)
		if len(page.Items) != 2 {
			return false
		}
		for _, p := range page.Items {
			if p.Status != "已确认安排，等待执行" || len(p.Next) != 1 || p.Next[0] != "跟进承诺" || len(p.Questions) != 0 || p.At.IsZero() || p.Session == "" {
				return false
			}
		}
		return true
	})
	e.settle("daytime-review")
	e.observe("daytime-review")
	return first, second
}

func TestDayShort(t *testing.T) {
	// 远端开发配置可能关闭自动调度或按 stdin 结束服务；验收必须覆盖这些继承值被覆盖的真实启动路径。
	t.Setenv("XINLI_AUTO_SCHEDULE", "false")
	t.Setenv("XINLI_EXIT_ON_STDIN_EOF", "true")
	e := newEnvironment(t, false)
	e.morning()
}

func TestDay(t *testing.T) {
	e := newEnvironment(t, false)
	first, second := e.morning()
	e.personaFaults()
	c := e.quote()
	if c.Rev < 2 || c.Due != "2035-01-10" {
		t.Fatal("afternoon model revision missing")
	}
	e.bffRequest("PATCH", "/v1/commitments/"+c.ID, map[string]any{"rev": c.Rev, "fields": map[string]string{"due_date": "2035-01-12"}}, 200, nil)
	edited := e.quote()
	if edited.Due != "2035-01-12" || edited.Rev <= c.Rev {
		t.Fatal("BFF edit was not applied")
	}
	e.observe("user-edit")

	e.stop("orchestrator")
	e.advance("2035-01-08T23:58:00+08:00")
	e.enqueue(businessDay, "闲聊：今天辛苦了。", "23:58:00")
	e.flush("")
	e.advance("2035-01-09T00:01:00+08:00")
	next := e.enqueue("2035-01-09", "闲聊：明早见。", "00:01:00")
	e.flush("")
	var duplicate receipt
	e.bffRequest("POST", "/v1/captures/batches", next, 200, &duplicate)
	if len(duplicate.Refs) != 1 || strings.TrimPrefix(duplicate.Refs[0], "#") != "1.0" {
		t.Fatal("cross-day reference sequence did not restart within the day")
	}
	e.advance("2035-01-09T04:31:00+08:00")
	e.start("orchestrator")
	e.organized(businessDay, 4)
	e.organized("2035-01-09", 1)
	eventually(t, "final task accepted fact", func() bool {
		return e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE source_kind='schedule_final' AND state='succeeded'`) > 0
	})
	e.settle("midnight-final")
	e.observe("midnight-final")

	e.assistant()
	e.outboxCrashes()
	// 三个角色分别退出与重建，BFF 登录态、账本和通知任务从 PG 恢复。
	for _, role := range []string{"persona", "bff", "orchestrator"} {
		e.start(role)
	}
	if got := e.quote(); got.ID != edited.ID || got.Due != edited.Due {
		t.Fatal("restart lost user edit")
	}
	e.settle("roles-restarted")
	e.observe("roles-restarted")

	e.erase(second)
	e.observe("erasure-complete")
	e.outboxExpiry()
	beforeExpiry := e.quote()
	var retainedDay struct{ Sessions []string }
	e.jsonRow(e.personaPool, `SELECT payload FROM business_days WHERE business_day=$1`, &retainedDay, []byte(businessDay))
	e.advance("2035-01-18T09:00:00+08:00")
	eventually(t, "natural expiry removed original text", func() bool { return e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`) == 0 })
	if got := e.quote(); got.ID != beforeExpiry.ID || got.Due != beforeExpiry.Due {
		t.Fatal("natural expiry changed ledger")
	}
	eventually(t, "expired kernel work Sessions cleaned", func() bool {
		for _, sid := range retainedDay.Sessions {
			events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
			must(t, err)
			if len(events) != 0 {
				return false
			}
		}
		return true
	})
	if !e.day(businessDay).Expired {
		t.Fatal("BFF did not expose natural expiry")
	}
	e.bffRequest("GET", "/v1/days/"+businessDay+"/captures", nil, 410, nil)
	code, _ := request(e.ctx, e.bff.server.URL+"/v1/captures/batches", "POST", e.token, first)
	if code != 409 && code != 410 {
		t.Fatal("expired data was accepted")
	}
	if e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`) != 0 {
		t.Fatal("expired upload revived text")
	}
	e.settle("natural-expiry")
	e.observe("natural-expiry")
}

func (e *environment) newThread() string {
	var out struct {
		ID string `json:"id"`
	}
	e.bffRequest("POST", "/v1/threads", map[string]any{}, 201, &out)
	if out.ID == "" {
		e.t.Fatal("thread ID missing")
	}
	return out.ID
}
func (e *environment) input(sid, text string) string {
	var out struct {
		ID string `json:"run_id"`
	}
	e.bffRequest("POST", "/v1/threads/"+sid+"/inputs", map[string]string{"input_id": randomID(e.t), "text": text}, 202, &out)
	if out.ID == "" {
		e.t.Fatal("Run ID missing")
	}
	return out.ID
}
func (e *environment) run(sid, id, status string) runView {
	e.t.Helper()
	var out runView
	eventually(e.t, "Run "+id+" "+status, func() bool {
		code, raw := request(e.ctx, e.kernel.URL+"/v1/sessions/"+sid, "GET", e.serviceToken, nil)
		if code != 200 {
			e.t.Fatal("kernel Session unavailable")
		}
		var view struct {
			Runs []runView `json:"runs"`
		}
		must(e.t, json.Unmarshal(raw, &view))
		for _, r := range view.Runs {
			if r.ID == id {
				out = r
				return r.Status == status
			}
		}
		return false
	})
	return out
}
func (e *environment) assistant() {
	sid := e.newThread()
	rid := e.input(sid, "call mcp_xinli__list_commitments {}\nask 要继续吗？ | 继续 | 结束\ncall approval_probe {}")
	waiting := e.run(sid, rid, "suspended")
	if waiting.Waiting == nil || waiting.Waiting.Kind != "question" {
		e.t.Fatal("assistant question missing")
	}
	e.bffRequest("POST", "/v1/threads/"+sid+"/answers/"+waiting.Waiting.Call, map[string]string{"text": "继续"}, 204, nil)
	eventually(e.t, "assistant approval", func() bool {
		waiting = e.run(sid, rid, "suspended")
		return waiting.Waiting != nil && waiting.Waiting.Kind == "approval"
	})
	e.bffRequest("POST", "/v1/threads/"+sid+"/approvals/"+waiting.Waiting.Call, map[string]bool{"approve": true}, 204, nil)
	e.run(sid, rid, "completed")
	// 对纯查询回复重答，避免重答再次产生需要用户回答的相同工具序列。
	answer := e.input(sid, "call mcp_xinli__list_commitments {}")
	e.run(sid, answer, "completed")
	var redo struct {
		ID string `json:"run_id"`
	}
	e.bffRequest("POST", "/v1/threads/"+sid+"/redo", map[string]string{"input_id": randomID(e.t), "expected_run_id": answer}, 202, &redo)
	e.run(sid, redo.ID, "completed")
	if redo.ID == answer {
		e.t.Fatal("redo reused Run")
	}
	events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
	must(e.t, err)
	queries := map[string]bool{}
	redoLinked, querySucceeded := false, false
	quote := e.quote()
	for _, event := range events {
		if requested := event.GetRunRequested(); requested.GetRunId() == redo.ID {
			redoLinked = requested.GetRedoOf() == answer
		}
		for _, call := range event.GetAssistantMessage().GetToolCalls() {
			if call.GetCapability() == "mcp_xinli__list_commitments" {
				queries[call.GetCallId()] = true
			}
		}
		if result := event.GetToolResult(); result != nil && result.GetRunId() == answer && queries[result.GetCallId()] {
			text := model.Text(result.GetContent())
			querySucceeded = !result.GetIsError() && strings.Contains(text, quote.ID) && strings.Contains(text, quote.Due)
		}
	}
	if !redoLinked || !querySucceeded {
		e.t.Fatal("assistant query or redo Event evidence missing")
	}
	e.bffRequest("POST", "/v1/threads/"+sid+"/redo", map[string]string{"input_id": randomID(e.t), "expected_run_id": answer}, 409, nil)
	due := e.morningSchedule(sid, "call mcp_xinli__list_commitments {}")
	e.advance(due.Add(time.Minute).Format(time.RFC3339))
	scheduled := e.scheduledRun(sid, due)
	e.run(sid, scheduled, "completed")
	e.waitSent(scheduled)
	e.settle("assistant-scheduled-inbox")
	e.observe("assistant-scheduled-inbox")
}
