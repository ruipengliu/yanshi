package day

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"yanshi/internal/eventlog"
	"yanshi/internal/outbox"
)

type deliveryState struct {
	Items map[string]struct {
		Run  string `json:"run_id"`
		Kind string `json:"kind"`
	}
	Jobs map[string]struct {
		State    string
		Attempts int
		Item     struct {
			Run  string `json:"run_id"`
			Kind string `json:"kind"`
		}
	}
}
type recoveryProgress struct {
	Phase     string
	Round     uint64
	Positions []struct {
		Shard int
		After string
	}
}

func (e *environment) delivery() deliveryState {
	var state deliveryState
	e.jsonRow(e.appPool, `SELECT payload FROM delivery_state WHERE business_line=$1 AND end_user=$2`, &state, businessLine, e.endUser)
	return state
}
func (e *environment) progress() recoveryProgress {
	var p recoveryProgress
	e.jsonRow(e.appPool, `SELECT payload FROM outbox_recovery WHERE business_line=$1`, &p, businessLine)
	return p
}
func (e *environment) follow() {
	eventually(e.t, "consumer follows outbox", func() bool { return e.progress().Phase == "follow" })
}
func (e *environment) waitSent(run string) {
	e.t.Helper()
	eventually(e.t, "durable inbox and fake delivery for Run "+run, func() bool {
		state := e.delivery()
		items, sent := 0, 0
		for _, item := range state.Items {
			if item.Run == run && item.Kind == "run_completed" {
				items++
			}
		}
		for _, job := range state.Jobs {
			if job.Item.Run == run && job.Item.Kind == "run_completed" && job.State == "sent" && job.Attempts > 0 {
				sent++
			}
		}
		if items > 1 || sent > 1 {
			e.t.Fatal("duplicate inbox item or notification job")
		}
		return items == 1 && sent == 1 && e.control.sent(run, "run_completed") == 1
	})
}

func (e *environment) personaFaults() {
	e.stop("orchestrator")
	e.advance("2035-01-08T14:00:00+08:00")
	third := e.enqueue(businessDay, "张总，办公室装修的报价单改到后天发给你。", "14:00:00")
	before := e.day(businessDay)
	e.dbProxy.cut(true)
	e.flush("unavailable")
	if e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`) != 2 {
		e.t.Fatal("DB outage accepted capture")
	}
	e.dbProxy.cut(false)
	if after := e.personaRecovered(businessDay); after.Revision != before.Revision || after.Progress != before.Progress {
		e.t.Fatal("DB outage advanced revision/progress")
	}

	barrier := e.control.arm(e.t, "persona.before_commit", "")
	result := make(chan int, 1)
	go func() {
		code, _ := request(e.ctx, e.bff.server.URL+"/v1/captures/batches", "POST", e.token, third)
		result <- code
	}()
	barrier.wait(e.t)
	barrier.reject.Store(true)
	barrier.open()
	select {
	case code := <-result:
		if code != 503 {
			e.t.Fatal("injected transaction did not roll back")
		}
	case <-time.After(10 * time.Second):
		e.t.Fatal("rollback request hung")
	}
	e.control.disarm("persona.before_commit")
	if after := e.personaRecovered(businessDay); after.Revision != before.Revision || after.Progress != before.Progress {
		e.t.Fatal("rolled back transaction advanced progress")
	}
	if e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE live`) != 2 {
		e.t.Fatal("rolled back transaction retained text")
	}
	e.flush("")
	if d := e.day(businessDay); d.Revision != 3 || d.Processed {
		e.t.Fatal("recovered upload was not pending processing")
	}
	e.observe("persona-db-recovered")

	// 扫描与通知是两条独立路径；先冻结扫描，才能准确证明通知前退出仍有持久后续。
	discovery := e.control.arm(e.t, "persona.discover_changes", "")
	filed := e.control.arm(e.t, "persona.file_day_committed", "")
	oldCollect := e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE operation='collect'`)
	e.start("orchestrator")
	discovery.wait(e.t)
	e.advance("2035-01-08T14:10:00+08:00")
	filed.wait(e.t)
	if e.count(e.personaPool, `SELECT count(*) FROM domain_changes WHERE status='pending'`) == 0 {
		e.t.Fatal("file_day committed without durable change")
	}
	if e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE operation='collect'`) != oldCollect {
		e.t.Fatal("fault missed before-notification boundary")
	}
	e.stop("persona")
	e.stop("orchestrator")
	e.control.disarm("persona.file_day_committed")
	e.control.disarm("persona.discover_changes")
	e.start("persona")
	e.expireTaskLeases()
	e.start("orchestrator")
	e.organized(businessDay, 3)
	eventually(e.t, "committed file_day recovered without notification", func() bool {
		return e.count(e.personaPool, `SELECT count(*) FROM domain_changes WHERE status='pending'`) == 0 &&
			e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE operation='collect' AND state='succeeded'`) > oldCollect
	})
	e.settle("file-day-crash-recovered")
	e.observe("file-day-crash-recovered")
}

func (e *environment) outboxCrashes() {
	for _, point := range []string{"consumer.before_commit", "consumer.before_process"} {
		e.follow()
		e.stop("orchestrator")
		before := e.progress()
		sid := e.newThread()
		run := e.input(sid, "call mcp_xinli__list_commitments {}")
		e.run(sid, run, "completed")
		barrier := e.control.arm(e.t, point, sid)
		e.start("orchestrator")
		barrier.wait(e.t)
		count := e.count(e.appPool, `SELECT count(*) FROM outbox_receipts WHERE session_id=$1`, sid)
		if point == "consumer.before_commit" {
			if count != 0 {
				e.t.Fatal("uncommitted envelope has a receipt")
			}
			current := e.progress()
			if len(current.Positions) != 1 {
				e.t.Fatal("consumer position missing")
			}
			page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, After: current.Positions[0].After,
				Kinds: deliveryKinds(), Limit: 1000})
			must(e.t, err)
			found := false
			for _, envelope := range page.Events {
				if envelope.RunID == run {
					found = true
				}
			}
			if !found {
				e.t.Fatal("cursor crossed uncommitted envelope")
			}
		} else {
			if count != 1 || e.count(e.appPool, `SELECT count(*) FROM outbox_receipts WHERE session_id=$1 AND NOT processed`, sid) != 1 {
				e.t.Fatal("receipt was not durably pending")
			}
			if reflect.DeepEqual(e.progress().Positions, before.Positions) {
				e.t.Fatal("receipt and cursor did not commit together")
			}
			for _, item := range e.delivery().Items {
				if item.Run == run {
					e.t.Fatal("fault missed before-process boundary")
				}
			}
		}
		e.stop("orchestrator")
		e.control.disarm(point)
		e.start("orchestrator")
		e.waitSent(run)
		e.settle(point + "-recovered")
		e.observe(point + "-recovered")
	}
}

func (e *environment) erase(old batch) {
	e.stop("orchestrator")
	before := e.day(businessDay)
	var day struct{ Session string }
	e.jsonRow(e.personaPool, `SELECT payload FROM business_days WHERE business_day=$1`, &day, []byte(businessDay))
	var pending struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
		ID string `json:"id"`
	}
	e.bffRequest("POST", "/v1/days/"+businessDay+"/erase", map[string]string{"from": "#2.0", "to": "#2.0"}, 202, &pending)
	after := e.day(businessDay)
	if after.Revision <= before.Revision || after.Processed {
		e.t.Fatal("erasure did not invalidate progress/revision")
	}
	var deletions struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	e.bffRequest("GET", "/v1/deletions", nil, 200, &deletions)
	if len(deletions.Items) == 0 || deletions.Items[0].Status != "deleting" {
		e.t.Fatal("BFF reported deletion complete before kernel cleanup")
	}
	e.bffRequest("POST", "/v1/captures/batches", old, 409, nil)
	// 从旧 Session 延迟交回同一 source，须被 Persona 的退役屏障拒收。
	beforeChanges := e.count(e.personaPool, `SELECT count(*) FROM changesets`)
	e.lateFileChanges(day.Session)
	if e.count(e.personaPool, `SELECT count(*) FROM changesets`) != beforeChanges {
		e.t.Fatal("late submission resurrected erased fact")
	}
	e.start("orchestrator")
	eventually(e.t, "erasure and kernel deletion complete", func() bool {
		e.bffRequest("GET", "/v1/deletions", nil, 200, &deletions)
		if len(deletions.Items) == 0 || deletions.Items[0].Status != "completed" {
			return false
		}
		code, _ := request(e.ctx, e.kernel.URL+"/v1/sessions/"+day.Session, "GET", e.serviceToken, nil)
		return code == 404
	})
	events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, day.Session, 0)
	must(e.t, err)
	if len(events) != 0 {
		e.t.Fatal("deletion completed while kernel original text copies remain")
	}
	if e.count(e.personaPool, `SELECT count(*) FROM capture_segments WHERE business_day=$1 AND batch_number=2 AND live`, businessDay) != 0 {
		e.t.Fatal("erased original text survived")
	}
	if got := e.quote(); got.Due != "2035-01-12" {
		e.t.Fatal("unrelated user edit lost after erasure")
	}
	e.organized(businessDay, after.Revision)
	e.settle("erasure-reprocessed")
}

func (e *environment) outboxExpiry() {
	e.follow()
	e.stop("orchestrator")
	before := e.progress()
	if len(before.Positions) != 1 {
		e.t.Fatal("expected one kernel database/shard")
	}
	sid := e.newThread()
	approval := e.input(sid, "call approval_probe {}")
	waiting := e.run(sid, approval, "suspended")
	if waiting.Waiting == nil || waiting.Waiting.Kind != "approval" {
		e.t.Fatal("approval not created")
	}
	// 消费者离线期间用户已处理；恢复器可以重建信封，但不得再推过时审批。
	e.bffRequest("POST", "/v1/threads/"+sid+"/approvals/"+waiting.Waiting.Call, map[string]bool{"approve": false}, 204, nil)
	e.run(sid, approval, "completed")
	fresh := e.input(sid, "call mcp_xinli__list_commitments {}")
	e.run(sid, fresh, "completed")
	e.published(fresh)
	must(e.t, e.janitor.Sweep(e.ctx))
	e.advance("2035-01-13T10:00:00+08:00")
	kinds := "approval_requested,question,run_completed,run_failed,session_deleted"
	path := "/v1/outbox?shard=0&kinds=" + kinds + "&after=" + before.Positions[0].After
	code, raw := request(e.ctx, e.kernel.URL+path, "GET", e.serviceToken, nil)
	if code != 410 || !strings.Contains(string(raw), "outbox_cursor_expired") {
		e.t.Fatal("expired outbox cursor did not fail explicitly")
	}
	if e.count(e.kernelPool, `SELECT count(*) FROM outbox WHERE session_id=$1`, sid) != 0 {
		e.t.Fatal("Janitor did not prune published envelopes")
	}
	e.start("orchestrator")
	eventually(e.t, "B5 recovery round reaches follow", func() bool { p := e.progress(); return p.Round > before.Round && p.Phase == "follow" })
	e.waitSent(fresh)
	eventually(e.t, "old approval notification cancelled", func() bool {
		for _, job := range e.delivery().Jobs {
			if job.Item.Run == approval && job.Item.Kind == "approval_requested" {
				return job.State == "cancelled"
			}
		}
		return false
	})
	if e.control.sent(approval, "approval_requested") != 0 {
		e.t.Fatal("stale approval was pushed")
	}
	e.settle("outbox-expired-reconciled")
	e.observe("outbox-expired-reconciled")
}

// 长事务在真实 PG 占用 xid；用完整 Run 产生信封，不向 outbox 表伪造业务事件。
func TestDayLongTransaction(t *testing.T) {
	e := newEnvironment(t, false)
	sid := e.newThread()
	head, err := e.core.Stores.Outbox.Head(e.ctx, outbox.Request{BusinessLine: businessLine})
	must(t, err)
	blocker, err := e.kernelPool.Begin(e.ctx)
	must(t, err)
	defer blocker.Rollback(e.ctx)
	_, err = blocker.Exec(e.ctx, `SELECT pg_current_xact_id()`)
	must(t, err)
	run := e.input(sid, "call mcp_xinli__list_commitments {}")
	e.run(sid, run, "completed")
	for _, at := range []string{"2035-01-08T10:00:00+08:00", "2035-01-12T10:00:00+08:00"} {
		e.advance(at)
		page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, After: head})
		must(t, err)
		if len(page.Events) != 0 {
			t.Fatal("watermark crossed long transaction")
		}
		if e.count(e.kernelPool, `SELECT count(*) FROM outbox WHERE session_id=$1`, sid) != 1 {
			t.Fatal("unpublished envelope expired")
		}
	}
	must(t, blocker.Rollback(e.ctx))
	eventually(t, "long transaction released watermark", func() bool {
		page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, After: head})
		must(t, err)
		return len(page.Events) == 1 && page.Events[0].RunID == run
	})
	must(t, e.janitor.Sweep(e.ctx))
	e.advance("2035-01-15T09:59:00+08:00")
	if e.count(e.kernelPool, `SELECT count(*) FROM outbox WHERE session_id=$1`, sid) != 1 {
		t.Fatal("publication retention shorter than 72h")
	}
	// B5 从日志恢复也须仍能找到这个 Run。
	events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
	must(t, err)
	if len(events) == 0 {
		t.Fatal("long transaction affected committed Run")
	}
	e.start("orchestrator")
	e.waitSent(run)
	e.advance("2035-01-15T10:01:00+08:00")
	if e.count(e.kernelPool, `SELECT count(*) FROM outbox WHERE session_id=$1`, sid) != 0 {
		t.Fatal("published envelope survived its 72h retention")
	}
}

func TestDayPersonaFaults(t *testing.T) {
	e := newEnvironment(t, false)
	e.morning()
	e.personaFaults()
}

func TestDayConsumerFaults(t *testing.T) {
	e := newEnvironment(t, false)
	e.morning()
	e.outboxCrashes()
	e.outboxExpiry()
}

func deliveryKinds() []outbox.Kind {
	return []outbox.Kind{outbox.ApprovalRequested, outbox.Question, outbox.RunCompleted, outbox.RunFailed, outbox.SessionDeleted}
}

func (e *environment) published(run string) {
	eventually(e.t, "Run envelope safely published", func() bool {
		page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, Limit: 1000})
		must(e.t, err)
		for _, envelope := range page.Events {
			if envelope.RunID == run {
				return true
			}
		}
		return false
	})
}
