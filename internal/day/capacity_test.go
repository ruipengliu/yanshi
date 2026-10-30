package day

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/outbox"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
)

type capacityDB struct {
	Shard           int   `json:"shard"`
	Sessions        int   `json:"sessions"`
	IndexedUsers    int   `json:"indexed_end_users"`
	Events          int   `json:"events"`
	Envelopes       int   `json:"envelopes"`
	Inserted        int64 `json:"inserted_tuples"`
	Updated         int64 `json:"updated_tuples"`
	Deleted         int64 `json:"deleted_tuples"`
	Connections     int   `json:"database_connections_at_end"`
	PeakConnections int32 `json:"peak_pool_connections"`
}
type capacityReport struct {
	Shards        int          `json:"shards"`
	Users         int          `json:"end_users"`
	Sessions      int          `json:"sessions"`
	Runs          int          `json:"runs"`
	Concurrency   int          `json:"client_concurrency"`
	ElapsedMS     int64        `json:"elapsed_ms"`
	RunsPerSecond float64      `json:"runs_per_second"`
	P50MS         float64      `json:"workflow_p50_ms"`
	P95MS         float64      `json:"workflow_p95_ms"`
	WALBytes      int64        `json:"instance_wal_bytes"`
	Databases     []capacityDB `json:"databases"`
	Passed        bool         `json:"passed"`
}

// 同一机器、相同配方与并发；两个 database 共用健康 PG 实例，WAL 只能报告实例总量。
// 这是分摊观察，不将串行客户端测得的吞吐称作饱和容量。
func TestShardCapacity(t *testing.T) {
	if os.Getenv("YANSHI_TEST_CAPACITY") != "1" {
		t.Skip("set YANSHI_TEST_CAPACITY=1; use make shard-capacity")
	}
	if testing.Short() {
		t.Skip("capacity is explicit")
	}
	failoverDSNs(t)
	const users, sessionsPerUser, runsPerSession = 16, 4, 3
	for _, multiple := range []bool{false, true} {
		shards := 1
		if multiple {
			shards = 2
		}
		t.Run(fmt.Sprintf("shards-%d", shards), func(t *testing.T) {
			r := &capacityReport{Shards: shards, Users: users, Sessions: users * sessionsPerUser, Runs: users * sessionsPerUser * runsPerSession, Concurrency: 1}
			t.Cleanup(func() { r.Passed = !t.Failed(); writeResult(t, fmt.Sprintf("capacity-%d", shards), r) })
			dsns := []string{pgtest.Database(t)}
			if multiple {
				dsns = append(dsns, pgtest.Database(t))
			}
			e := newEnvironmentConfigured(t, false, multiple, "", dsns)
			var startLSN string
			must(t, e.kernelPool.QueryRow(e.ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&startLSN))
			var mu sync.Mutex
			peaks := map[int]int32{}
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					mu.Lock()
					for id, pool := range e.shardPools {
						peaks[id] = max(peaks[id], pool.Stat().TotalConns())
					}
					mu.Unlock()
					select {
					case <-stop:
						return
					case <-ticker.C:
					}
				}
			}()
			defer func() { close(stop); <-done }()
			cursors := map[int]string{}
			for id := range e.shardPools {
				head, err := e.core.Stores.Outbox.Head(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: id})
				must(t, err)
				cursors[id] = head
			}
			before := map[int]capacityDB{}
			for id, pool := range e.shardPools {
				before[id] = readCapacityDB(t, pool, id)
			}
			var latencies []float64
			started := time.Now()
			for u := range users {
				owner := fmt.Sprintf("capacity-user-%03d", u)
				for range sessionsPerUser {
					code, raw := e.kernelRequest("POST", "/v1/sessions", map[string]string{"end_user": owner, "agent": "xinli-assistant"}, nil)
					if code != 201 {
						t.Fatalf("capacity create: %d", code)
					}
					var created struct {
						ID string `json:"session_id"`
					}
					must(t, json.Unmarshal(raw, &created))
					id := 0
					if multiple {
						var err error
						id, err = e.locator.Locate(shard.Target{Kind: shard.SessionData, Key: created.ID})
						must(t, err)
					}
					for range runsPerSession {
						at := time.Now()
						run := e.kernelInput(created.ID, "call failover_probe {}")
						e.run(created.ID, run, "completed")
						found := false
						eventually(t, "capacity outbox consumption", func() bool {
							page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: id, After: cursors[id], Limit: 1000})
							must(t, err)
							for _, row := range page.Events {
								if row.RunID == run {
									found = true
								}
							}
							cursors[id] = page.Next
							return found
						})
						latencies = append(latencies, float64(time.Since(at).Microseconds())/1000)
					}
				}
			}
			elapsed := time.Since(started)
			r.ElapsedMS, r.RunsPerSecond = elapsed.Milliseconds(), float64(r.Runs)/elapsed.Seconds()
			sort.Float64s(latencies)
			r.P50MS = latencies[(len(latencies)-1)*50/100]
			r.P95MS = latencies[(len(latencies)-1)*95/100]
			must(t, e.kernelPool.QueryRow(e.ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(),$1::pg_lsn)::bigint`, startLSN).Scan(&r.WALBytes))
			// PG 的统计异步发布，等一轮后读取；不把表行数当 UPDATE/WAL 总写入量。
			timer := time.NewTimer(2 * time.Second)
			<-timer.C
			for id := 0; id < shards; id++ {
				db := readCapacityDB(t, e.shardPools[id], id)
				db.Inserted -= before[id].Inserted
				db.Updated -= before[id].Updated
				db.Deleted -= before[id].Deleted
				mu.Lock()
				db.PeakConnections = peaks[id]
				mu.Unlock()
				r.Databases = append(r.Databases, db)
			}
			totalSessions, totalEnvelopes := 0, 0
			for _, db := range r.Databases {
				totalSessions += db.Sessions
				totalEnvelopes += db.Envelopes
			}
			if totalSessions != r.Sessions || totalEnvelopes != r.Runs {
				t.Fatal("fixed load lost Session or completion envelope")
			}
			e.effects.verify(t)
			e.effects.mu.Lock()
			effects := len(e.effects.calls)
			e.effects.mu.Unlock()
			if effects != r.Runs {
				t.Fatal("fixed load did not execute one tool per Run")
			}
		})
	}
}

func readCapacityDB(t *testing.T, pool *pgxpool.Pool, id int) capacityDB {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := capacityDB{Shard: id}
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()`).Scan(&out.Connections))
	must(t, pool.QueryRow(ctx, `SELECT count(DISTINCT end_user) FROM sessions`).Scan(&out.IndexedUsers))
	must(t, pool.QueryRow(ctx, `SELECT count(DISTINCT session_id),count(*) FROM events`).Scan(&out.Sessions, &out.Events))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind='run_completed'`).Scan(&out.Envelopes))
	must(t, pool.QueryRow(ctx, `SELECT coalesce(sum(n_tup_ins),0)::bigint,coalesce(sum(n_tup_upd),0)::bigint,coalesce(sum(n_tup_del),0)::bigint FROM pg_stat_user_tables WHERE schemaname=current_schema()`).Scan(&out.Inserted, &out.Updated, &out.Deleted))
	return out
}
