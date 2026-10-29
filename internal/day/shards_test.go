package day

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"yanshi/internal/eventlog"
	"yanshi/internal/shard"
)

// 两个真实内核库、独立 xinli 进程；用脚本模型避免把模型速度计入分片验收。
func TestDayShards(t *testing.T) {
	started := time.Now()
	e := newEnvironmentShards(t, false, true)
	e.start("orchestrator")
	where := func(sid string) int {
		id, err := e.locator.Locate(shard.Target{Kind: shard.SessionData, Key: sid})
		must(t, err)
		return id
	}
	threads := map[int]string{}
	for i := 0; len(threads) < 2 && i < 32; i++ {
		sid := e.newThread()
		threads[where(sid)] = sid
	}
	if len(threads) != 2 {
		t.Fatal("Sessions did not cover both shards")
	}
	for id := range 2 {
		sid := threads[id]
		run := e.input(sid, "你好")
		e.run(sid, run, "completed")
		e.waitSent(run)
	}
	eventually(t, "independent persisted shard cursors", func() bool {
		return e.count(e.appPool, `SELECT count(*) FROM outbox_recovery WHERE business_line=$1 AND shard IN (0,1)
			AND payload->>'Phase'='follow' AND (payload->>'Version')::bigint=version
			AND (payload->>'Revision')::bigint>0 AND jsonb_array_length(payload->'Positions')=1
			AND (payload->'Positions'->0->>'Shard')::integer=shard
			AND COALESCE(payload->'Positions'->0->>'After','')<>''`, businessLine) == 2
	})
	// 切断实际 SQL/LISTEN TCP 连接，健康目标的 EndUser 依赖也必须留在健康片。
	ownerShard, err := e.locator.Locate(shard.Target{Kind: shard.EndUser, Key: e.endUser})
	must(t, err)
	bad := 1 - ownerShard
	e.shardProxies[bad].cut(true)
	defer e.shardProxies[bad].cut(false)
	failedAt := time.Now()
	code, _ := request(e.ctx, e.kernel.URL+"/v1/sessions/"+threads[bad], http.MethodGet, e.serviceToken, nil)
	if code != http.StatusServiceUnavailable || time.Since(failedAt) > 3*time.Second {
		t.Fatalf("failed shard was not bounded: %d", code)
	}
	goodRun := e.input(threads[ownerShard], "故障期间继续")
	e.run(threads[ownerShard], goodRun, "completed")
	e.waitSent(goodRun)
	createdHealthy := false
	for range 32 {
		code, _ := request(e.ctx, e.kernel.URL+"/v1/sessions", http.MethodPost, e.serviceToken, map[string]string{"end_user": e.endUser, "agent": "xinli-assistant"})
		if code == http.StatusCreated {
			createdHealthy = true
			break
		}
		if code != http.StatusServiceUnavailable {
			t.Fatalf("healthy creation: %d", code)
		}
	}
	if !createdHealthy {
		t.Fatal("healthy shard could not create a Session")
	}
	e.shardProxies[bad].cut(false)
	// 重连与消费者退避各自恢复，不重建存储或清空已有游标。
	eventually(t, "failed shard reconnects", func() bool {
		code, _ := request(e.ctx, e.kernel.URL+"/v1/sessions/"+threads[bad], http.MethodGet, e.serviceToken, nil)
		return code == http.StatusOK
	})
	recoveredRun := e.input(threads[bad], "恢复后继续")
	e.run(threads[bad], recoveredRun, "completed")
	e.waitSent(recoveredRun)
	crossed := false
	var parent, child string
	for attempt := 0; attempt < 32 && !crossed; attempt++ {
		var created struct {
			ID string `json:"session_id"`
		}
		code, raw := request(e.ctx, e.kernel.URL+"/v1/sessions", http.MethodPost, e.serviceToken, map[string]string{"end_user": e.endUser, "agent": "shard-parent"})
		if code != http.StatusCreated {
			t.Fatalf("create parent: %d", code)
		}
		must(t, json.Unmarshal(raw, &created))
		parent = created.ID
		var submitted struct {
			ID string `json:"run_id"`
		}
		code, raw = request(e.ctx, e.kernel.URL+"/v1/sessions/"+parent+"/inputs", http.MethodPost, e.serviceToken, map[string]string{"text": `call agent__shard-helper {"task":"跨片"}`})
		if code != http.StatusAccepted {
			t.Fatalf("submit parent: %d", code)
		}
		must(t, json.Unmarshal(raw, &submitted))
		e.run(parent, submitted.ID, "completed")
		rows, _, err := e.core.Stores.Index.List(e.ctx, businessLine, e.endUser, "", 200)
		must(t, err)
		for _, row := range rows {
			if row.Agent != "shard-helper" {
				continue
			}
			events, err := e.core.Stores.Log.Read(e.ctx, row.ID, 0, 1)
			must(t, err)
			if len(events) > 0 && events[0].GetSessionCreated().GetParent().GetSessionId() == parent {
				child = row.ID
				crossed = where(child) != where(parent)
			}
		}
	}
	if !crossed {
		t.Fatal("delegation did not cross shards")
	}
	code, _ = request(e.ctx, e.kernel.URL+"/v1/sessions/"+parent, http.MethodDelete, e.serviceToken, nil)
	if code >= 300 {
		t.Fatalf("delete parent: %d", code)
	}
	eventually(t, "cross-shard child physically deleted", func() bool {
		events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, child, 0)
		return err == nil && len(events) == 0
	})
	var deletion struct {
		ID string `json:"request_id"`
	}
	code, raw := request(e.ctx, e.kernel.URL+"/v1/end_users/"+e.endUser, http.MethodDelete, e.serviceToken, nil)
	if code != http.StatusAccepted {
		t.Fatalf("delete EndUser: %d", code)
	}
	must(t, json.Unmarshal(raw, &deletion))
	eventually(t, "cross-shard account deletion completes", func() bool {
		_ = e.janitor.Sweep(e.ctx)
		var progress struct {
			Done                bool
			Sessions, Completed int
		}
		code, raw := request(e.ctx, e.kernel.URL+"/v1/deletions/"+deletion.ID, http.MethodGet, e.serviceToken, nil)
		if code != http.StatusOK {
			return false
		}
		must(t, json.Unmarshal(raw, &progress))
		barrier, err := e.core.Stores.Deletions.GetEndUser(e.ctx, businessLine, e.endUser)
		return progress.Done && progress.Sessions == progress.Completed && err == nil && !barrier.CompletedAt.IsZero()
	})
	elapsed := time.Since(started)
	t.Logf("dual-shard day elapsed: %s", elapsed)
	if elapsed >= time.Minute {
		t.Fatalf("dual-shard check budget exceeded: %s", elapsed)
	}
}
