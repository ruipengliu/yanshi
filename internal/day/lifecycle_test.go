package day

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"yanshi/internal/eventlog"
	"yanshi/internal/outbox"
	"yanshi/internal/pg"
	"yanshi/internal/session"
)

// 仅恢复边界与状态轮询可以重试只读请求；提交类请求保留原来的单次断言。
func (e *environment) readBFF(path string, out any) bool {
	e.t.Helper()
	e.ensureAuth()
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
	defer cancel()
	code, raw := request(ctx, e.bff.server.URL+path, http.MethodGet, e.token, nil)
	if code == 0 || code == http.StatusServiceUnavailable {
		return false
	}
	if code != http.StatusOK {
		e.t.Fatalf("GET %s: status %d want 200", path, code)
	}
	if out != nil {
		must(e.t, json.Unmarshal(raw, out))
	}
	return true
}

func (e *environment) awaitBFFRead(label, path string, out any) {
	e.t.Helper()
	eventually(e.t, label, func() bool { return e.readBFF(path, out) })
}

func (e *environment) personaRecovered(date string) dayView {
	e.t.Helper()
	var day dayView
	// 父测试只负责观察，不参与服务恢复。丢弃它的旧连接，不能把观察连接的 EOF 当成业务丢数据。
	e.observationPoolRecovered()
	// 查询走 Persona 进程的池，采集同步走 BFF 自己的池；两条生产链路仍各自证明恢复。
	e.awaitBFFRead("BFF reads Persona after recovery", "/v1/days/"+date, &day)
	e.awaitBFFRead("BFF capture store recovers", "/v1/captures/sync?business_day="+date+"&session_id=capture-day", nil)
	return day
}

func (e *environment) observationPoolRecovered() {
	e.t.Helper()
	e.personaPool.Reset()
	eventually(e.t, "parent Persona observation pool reconnects", func() bool {
		ctx, cancel := context.WithTimeout(e.ctx, time.Second)
		defer cancel()
		var one int
		err := e.personaPool.QueryRow(ctx, `SELECT 1`).Scan(&one)
		if err != nil && !pg.IsUnavailable(err) {
			e.t.Fatal(err)
		}
		return err == nil && one == 1
	})
}

func (e *environment) roleReady(p *process) {
	e.t.Helper()
	eventually(e.t, "role readiness: "+p.role, func() bool {
		if !p.running() {
			e.t.Fatal("role exited before ready: " + p.role)
		}
		ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
		defer cancel()
		code, _ := request(ctx, p.health+"/readyz", http.MethodGet, "", nil)
		if code == 0 || code == http.StatusServiceUnavailable {
			return false
		}
		if code != http.StatusOK {
			e.t.Fatalf("role=%s readiness status=%d", p.role, code)
		}
		return true
	})
}

// 进度可以先于 file_day 更新，总览也可能来自上轮；必须关联当前接受事实和真实编排命令。
func (e *environment) dayFiled(date string, revision uint64, day dayView) bool {
	if !day.Processed || day.Revision != revision || len(day.Summary) == 0 || string(day.Summary) == "null" {
		return false
	}
	var fact struct {
		Value struct {
			Session, CommandID string
			Active             bool
			AcceptedAt         time.Time `json:"accepted_at"`
		}
	}
	if !e.jsonRow(e.personaPool, `SELECT payload FROM accepted_facts WHERE business_line=$1 AND end_user=$2 AND fact_key=$3`, &fact, []byte(businessLine), []byte(e.endUser), []byte("day/"+date)) {
		return false
	}
	if !fact.Value.Active || fact.Value.CommandID == "" {
		return false
	}
	var summary struct {
		Session string    `json:"session"`
		Filed   time.Time `json:"filed_at"`
	}
	must(e.t, json.Unmarshal(day.Summary, &summary))
	if summary.Session != fact.Value.Session || !summary.Filed.Equal(fact.Value.AcceptedAt) {
		return false
	}
	var metadata struct{ LastBatchAt time.Time }
	if !e.jsonRow(e.personaPool, `SELECT payload FROM business_days WHERE business_line=$1 AND end_user=$2 AND business_day=$3`, &metadata, []byte(businessLine), []byte(e.endUser), []byte(date)) {
		return false
	}
	if fact.Value.AcceptedAt.Before(metadata.LastBatchAt) {
		return false
	}
	return e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE business_line=$1 AND end_user=$2 AND input_id=$3 AND business_date=$4 AND target='day/'||$4 AND operation IN ('day','rebuild')`, businessLine, e.endUser, fact.Value.CommandID, date) == 1
}

// 强杀不会撤销持久租约；显式推进业务时钟到可接管点，不能用真实 45 秒等待替代两分钟租约。
func (e *environment) expireTaskLeases() {
	e.t.Helper()
	if p := e.processes["orchestrator"]; p != nil && p.running() {
		e.t.Fatal("lease recovery requires a stopped orchestrator")
	}
	var at *time.Time
	must(e.t, e.appPool.QueryRow(e.ctx, `SELECT max(greatest(retry_at,lease_until)) FROM orchestration_tasks WHERE state IN ('pending','unknown','accepted')`).Scan(&at))
	if at != nil && !at.Before(e.control.Now()) {
		e.t.Log("recovery advances business clock to persisted task retry/lease boundary")
		e.advance(at.Add(time.Second).Format(time.RFC3339Nano))
	}
}

func (e *environment) settled() bool {
	if e.count(e.appPool, `SELECT count(*) FROM orchestration_tasks WHERE state IN ('pending','unknown','accepted')`) != 0 ||
		e.count(e.personaPool, `SELECT count(*) FROM domain_changes WHERE status='pending'`) != 0 ||
		e.count(e.personaPool, `SELECT count(*) FROM deletion_tasks WHERE status='pending' OR rebuild_pending`) != 0 ||
		e.count(e.appPool, `SELECT count(*) FROM outbox_receipts WHERE NOT processed`) != 0 {
		return false
	}
	if !e.workStopped() {
		return false
	}
	progress := e.progress()
	if progress.Phase != "follow" || len(progress.Positions) != 1 {
		return false
	}
	page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: progress.Positions[0].Shard, After: progress.Positions[0].After, Kinds: deliveryKinds(), Limit: 1})
	if errors.Is(err, outbox.ErrExpired) {
		return false
	}
	must(e.t, err)
	if len(page.Events) != 0 {
		return false
	}
	for _, job := range e.delivery().Jobs {
		if job.State != "sent" && job.State != "cancelled" {
			return false
		}
	}
	return true
}

func (e *environment) workStopped() bool {
	// 接受事实可先于 Run 的终态；退役后也要等迟到工具结果了结，避免串入下一轮消费者故障。
	rows, err := e.personaPool.Query(e.ctx, `SELECT convert_from(session_id,'UTF8') FROM work_sessions ORDER BY session_id`)
	must(e.t, err)
	var ids []string
	for rows.Next() {
		var id string
		must(e.t, rows.Scan(&id))
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	must(e.t, err)
	for _, id := range ids {
		events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, id, 0)
		must(e.t, err)
		state, err := session.Reduce(events)
		must(e.t, err)
		if state.Active() != nil {
			return false
		}
	}
	return true
}

func (e *environment) settle(label string) {
	e.t.Helper()
	eventually(e.t, "phase settled: "+label, e.settled)
}

func TestDayRecoveryWaitRequiresSuccessfulRead(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"recovered":true}`))
	}))
	defer server.Close()
	e := &environment{t: t, ctx: context.Background(), bff: &frontProxy{server: server}}
	var result struct {
		Recovered bool `json:"recovered"`
	}
	e.awaitBFFRead("test recovery", "/v1/days/2035-01-08", &result)
	if !result.Recovered || attempts.Load() != 3 {
		t.Fatal("recovery wait accepted an unavailable response")
	}
}
