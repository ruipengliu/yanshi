package day

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"yanshi/internal/eventlog"
)

func TestFailoverPersona(t *testing.T) {
	if os.Getenv("YANSHI_TEST_FAILOVER") != "1" {
		t.Skip("set YANSHI_TEST_FAILOVER=1; use make failover")
	}
	primary, standby := failoverDSNs(t)
	reports := []*failoverReport{}
	defer func() { writeResult(t, "persona", reports) }()
	for _, mode := range []string{"writes", "deletions"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "deletions" {
				composeFailover(t, "down", "-v", "--remove-orphans")
				composeFailover(t, "up", "-d", "--wait")
				primary, standby = failoverDSNs(t)
			}
			r := &failoverReport{Kind: "persona-" + mode, Requests: map[string]int{}}
			reports = append(reports, r)
			started := time.Now()
			t.Cleanup(func() { r.ElapsedMS = time.Since(started).Milliseconds(); r.Passed = !t.Failed() })
			proxy, dsn := newTCPProxy(t, primary)
			e := newEnvironmentConfigured(t, false, false, dsn, nil)
			first, second := e.morning()
			c := e.quote()
			e.bffRequest("PATCH", "/v1/commitments/"+c.ID, map[string]any{"rev": c.Rev, "fields": map[string]string{"due_date": "2035-01-12"}}, 200, nil)
			edited := e.quote()
			if mode == "deletions" {
				e.stop("orchestrator")
				var binding struct{ Session string }
				e.jsonRow(e.personaPool, `SELECT payload FROM business_days WHERE business_day=$1`, &binding, []byte(businessDay))
				e.bffRequest("POST", "/v1/days/"+businessDay+"/erase", map[string]string{"from": "#2.0", "to": "#2.0"}, 202, nil)
				pending := e.count(e.personaPool, `SELECT count(*) FROM deletion_tasks WHERE status='pending' OR rebuild_pending`)
				if pending == 0 {
					t.Fatal("no durable deletion to replicate")
				}
				before := e.day(businessDay)
				var pendingRows json.RawMessage
				e.jsonRow(e.personaPool, `SELECT jsonb_agg(to_jsonb(d) ORDER BY task_key) FROM deletion_tasks d WHERE status='pending' OR rebuild_pending`, &pendingRows)
				switchDatabase(t, proxy, standby, func() { e.personaUnavailable(r) }, r)
				recovery := time.Now()
				after := e.personaRecovered(businessDay)
				r.RecoveryMS = time.Since(recovery).Milliseconds()
				var recoveredRows json.RawMessage
				e.jsonRow(e.personaPool, `SELECT jsonb_agg(to_jsonb(d) ORDER BY task_key) FROM deletion_tasks d WHERE status='pending' OR rebuild_pending`, &recoveredRows)
				if !reflect.DeepEqual(pendingRows, recoveredRows) {
					t.Fatal("replicated deletion identity/payload changed")
				}
				if after.Revision != before.Revision || after.Progress != before.Progress || e.count(e.personaPool, `SELECT count(*) FROM deletion_tasks WHERE status='pending' OR rebuild_pending`) != pending {
					t.Fatal("pending deletion/progress lost")
				}
				e.start("orchestrator")
				eventually(t, "replicated deletion completes", func() bool {
					return e.count(e.personaPool, `SELECT count(*) FROM deletion_tasks WHERE status='pending' OR rebuild_pending`) == 0
				})
				eventually(t, "replicated deletion removes Event data", func() bool {
					events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, binding.Session, 0)
					return err == nil && len(events) == 0
				})
				e.bffRequest("POST", "/v1/captures/batches", second, 409, nil)
				e.organized(businessDay, after.Revision)
				if e.quote().Due != edited.Due {
					t.Fatal("confirmed edit lost during deletion recovery")
				}
				return
			}
			// 已提交的 file_day 在通知之前停住，提升时仍有真实 MCP 调用在途。
			e.stop("orchestrator")
			e.advance("2035-01-08T14:00:00+08:00")
			third := e.enqueue(businessDay, "闲聊：今天辛苦了。", "14:00:00")
			e.flush("")
			filed := e.control.arm(t, "persona.file_day_committed", "")
			e.control.arm(t, "persona.discover_changes", "")
			e.start("orchestrator")
			e.advance("2035-01-08T14:10:00+08:00")
			filed.wait(t)
			before := e.day(businessDay)
			if !before.Processed || before.Revision != 3 {
				t.Fatal("file_day boundary did not commit progress")
			}
			var accepted json.RawMessage
			e.jsonRow(e.personaPool, `SELECT payload FROM accepted_facts WHERE business_line=$1 AND end_user=$2 AND fact_key=$3`, &accepted, []byte(businessLine), []byte(e.endUser), []byte("day/"+businessDay))
			if len(accepted) == 0 {
				t.Fatal("file_day acceptance missing")
			}
			// 下一批在真实事务提交前断开；使用同 ID 重投，不把超时当未提交证明。
			confirmedRefs := map[string][]string{}
			for _, b := range []batch{first, third} {
				var receipt receipt
				e.bffRequest("POST", "/v1/captures/batches", b, 200, &receipt)
				confirmedRefs[b.CaptureID] = receipt.Refs
			}
			fourth := e.enqueue(businessDay, "闲聊：下午继续。", "14:10:00")
			// 版本标记区分第四批与编排器的后台更新，不把任意事务误当目标提交。
			beforeCommit := e.control.arm(t, "persona.before_commit", `"revision":4`)
			result := make(chan int, 1)
			go func() {
				ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
				defer cancel()
				code, _ := observedRequest(ctx, e.bff.server.URL+"/v1/captures/batches", "POST", e.token, fourth, r)
				result <- code
			}()
			beforeCommit.wait(t)
			switchDatabase(t, proxy, standby, func() {
				e.personaUnavailable(r)
				ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
				defer cancel()
				code, _ := observedRequest(ctx, e.bff.server.URL+"/v1/commitments/"+edited.ID, "PATCH", e.token, map[string]any{"rev": edited.Rev, "fields": map[string]string{"due_date": "2035-01-13"}}, r)
				if code != 503 {
					t.Fatalf("edit during failover: %d", code)
				}
			}, r)
			e.control.disarm("persona.before_commit")
			select {
			case code := <-result:
				if code != 0 && code != 503 {
					t.Fatalf("unknown upload status: %d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("upload hung")
			}
			recovery := time.Now()
			after := e.personaRecovered(businessDay)
			r.RecoveryMS = time.Since(recovery).Milliseconds()
			if after.Revision != before.Revision || after.Progress != before.Progress || !reflect.DeepEqual(after.Summary, before.Summary) {
				t.Fatal("confirmed progress/summary lost or uncommitted upload accepted")
			}
			var afterAccepted json.RawMessage
			e.jsonRow(e.personaPool, `SELECT payload FROM accepted_facts WHERE business_line=$1 AND end_user=$2 AND fact_key=$3`, &afterAccepted, []byte(businessLine), []byte(e.endUser), []byte("day/"+businessDay))
			if !reflect.DeepEqual(accepted, afterAccepted) {
				t.Fatal("file_day acceptance changed")
			}
			for _, b := range []batch{first, third, fourth} {
				var one, two receipt
				e.bffRequest("POST", "/v1/captures/batches", b, 200, &one)
				e.bffRequest("POST", "/v1/captures/batches", b, 200, &two)
				if old, ok := confirmedRefs[b.CaptureID]; ok && !reflect.DeepEqual(old, one.Refs) {
					t.Fatal("confirmed reference changed")
				}
				if !reflect.DeepEqual(one.Refs, two.Refs) || len(one.Refs) != 1 {
					t.Fatal("same upload ID changed references")
				}
			}
			if got := e.quote(); got.Due != edited.Due || got.Rev != edited.Rev {
				t.Fatal("confirmed user edit lost")
			}
			// 同 rev 的编辑遇到未知结果时先查状态；不能拿新 rev 盲目重写。
			c = e.quote()
			e.bffRequest("PATCH", "/v1/commitments/"+c.ID, map[string]any{"rev": c.Rev, "fields": map[string]string{"due_date": "2035-01-13"}}, 200, nil)
			e.control.disarm("persona.file_day_committed")
			e.control.disarm("persona.discover_changes")
			e.stop("orchestrator")
			e.expireTaskLeases()
			e.start("orchestrator")
			e.advance("2035-01-08T15:00:00+08:00")
			e.organized(businessDay, 4)
			e.settle("persona-failover-recovered")
			e.observe("persona-failover-recovered")
		})
	}
}

func (e *environment) personaUnavailable(report *failoverReport) {
	for _, path := range []string{"/v1/days/" + businessDay, "/v1/captures/sync?business_day=" + businessDay + "&session_id=capture-day"} {
		ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
		code, _ := observedRequest(ctx, e.bff.server.URL+path, "GET", e.token, nil, report)
		cancel()
		if code != 503 {
			e.t.Fatalf("Persona unavailable must be bounded 503: %d", code)
		}
	}
}
