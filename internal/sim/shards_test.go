package sim

import (
	"context"
	"flag"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/usage"
	"yanshi/internal/workqueue/pgqueue"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
)

var shardSeeds = flag.Int("sim.shardseeds", 3, "单库、内存双分片与 PostgreSQL 双库差分种子数")

func simulationLocator(t *testing.T) shard.Locator {
	t.Helper()
	l, err := shard.ParseDeployment([]byte(`{"version":"sim-dual","table":{"shards":[0,1],"ranges":[{"first":0,"last":511,"shard":0},{"first":512,"last":1023,"shard":1}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestShardedMemoryMatchesSingle 不需要 PostgreSQL；现有单库 Fingerprint 路径与随机流保持原样。
func TestShardedMemoryMatchesSingle(t *testing.T) {
	for seed := uint64(1); seed <= uint64(*shardSeeds); seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { t.Parallel(); runShardSeed(t, seed, nil) })
	}
}

// 两个 database 独立拥有通知、迁移、序列与事务；schema 只用于同一 database 内的测试隔离。
func TestShardedPostgresMatchesMemory(t *testing.T) {
	pgtest.DSN(t)
	dsns := []string{pgtest.Database(t), pgtest.Database(t)}
	var delegations, users, parked, promoted, completed atomic.Int64
	t.Cleanup(func() {
		t.Logf("cross-shard delegations=%d users=%d far parks=%d promotions=%d", delegations.Load(), users.Load(), parked.Load(), promoted.Load())
		if *shardSeeds >= 3 && completed.Load() == int64(*shardSeeds) && (delegations.Load() == 0 || users.Load() == 0 || parked.Load() == 0 || promoted.Load() == 0) {
			t.Error("cross-shard coverage is empty")
		}
	})
	for seed := uint64(1); seed <= uint64(*shardSeeds); seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			stats := runShardSeed(t, seed, dsns)
			delegations.Add(int64(stats.CrossShardDelegations))
			users.Add(int64(stats.CrossShardUserSessions))
			parked.Add(int64(stats.FarParks))
			promoted.Add(int64(stats.DuePromotions))
			completed.Add(1)
		})
	}
}

// 每个种子占两个 database 的连接，至多四对；单库 PG 种子组在另一个顶层测试中运行。
var shardPGSlots = make(chan struct{}, 4)

func runShardSeed(t *testing.T, seed uint64, dsns []string) Stats {
	t.Helper()
	locator := simulationLocator(t)
	opts := Options{CompareShards: true, Seed: seed, Workers: 3, Sessions: 4, Ticks: 300,
		Faults: true, Redo: seed%2 == 0, Tiers: true, Schedules: seed%3 == 0, MaintainDues: true}
	if seed%3 == 0 {
		opts.LongRuns, opts.Ticks = true, 800
	}
	run := func(label string, opts Options) *World {
		w, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Run(); err != nil {
			t.Fatalf("seed %d %s: %v", seed, label, err)
		}
		return w
	}
	single := run("single memory", opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opts.NewStores = func(c clock.Clock) Stores {
		parts := map[int]app.Stores{0: app.MemoryShard(c, locator), 1: app.MemoryShard(c, locator)}
		stores, err := app.Sharded(ctx, locator, parts, app.ShardOptions{DeterministicQueues: true})
		if err != nil {
			t.Fatal(err)
		}
		return stores
	}
	dual := run("dual memory", opts)
	if single.Fingerprint() != dual.Fingerprint() {
		t.Fatalf("seed %d: single/dual memory diverged", seed)
	}
	if err := dual.countShardCoverage(locator); err != nil {
		t.Fatal(err)
	}
	if dual.Stats.ShardsWithSessions != 2 {
		t.Fatal("simulation did not use both shards")
	}
	t.Logf("shards=%d cross-delegations=%d cross-user-sessions=%d", dual.Stats.ShardsWithSessions, dual.Stats.CrossShardDelegations, dual.Stats.CrossShardUserSessions)
	if dsns == nil {
		return dual.Stats
	}
	shardPGSlots <- struct{}{}
	t.Cleanup(func() { <-shardPGSlots })
	p0, n0 := pgtest.FreshAt(t, dsns[0])
	p1, n1 := pgtest.FreshAt(t, dsns[1])
	// 后登记取消，使转发通知先于底层 Notifier/连接池停止。
	pgctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	var dues []*countedDues
	opts.NewStores = func(c clock.Clock) Stores {
		parts := map[int]app.Stores{0: app.PostgresShard(p0, n0, c, locator, 0), 1: app.PostgresShard(p1, n1, c, locator, 1)}
		for id := range 2 {
			part := parts[id]
			pool := p0
			if id == 1 {
				pool = p1
			}
			q := &countedDues{Queue: part.Queue.(*pgqueue.Queue).WithHorizon(time.Second), pool: pool}
			dues = append(dues, q)
			part.Queue = q
			parts[id] = part
		}
		stores, err := app.Sharded(pgctx, locator, parts, app.ShardOptions{DeterministicQueues: true})
		if err != nil {
			t.Fatal(err)
		}
		return stores
	}
	onPG := run("dual postgres", opts)
	if dual.Fingerprint() != onPG.Fingerprint() {
		t.Fatalf("seed %d: dual memory/postgres diverged", seed)
	}
	for _, q := range dues {
		dual.Stats.FarParks += q.parked
		dual.Stats.DuePromotions += q.promoted
	}
	// Event 指纹不含用量；跨库也核对原始幂等身份，匿名化不能新增第二笔。
	type identity struct {
		ID, Key, BusinessLine, EndUser string
		Cost                           int64
	}
	var expected, actual []identity
	for _, part := range dual.stores.Shards() {
		for _, e := range part.Usage.(*usage.Mem).Entries() {
			expected = append(expected, identity{e.ID, e.DedupKey(), e.BusinessLine, e.EndUser, e.Cost})
		}
	}
	for _, pool := range []*pgxpool.Pool{p0, p1} {
		rows, err := pool.Query(context.Background(), `SELECT id,idempotency_key,business_line,end_user,cost FROM usage_entries`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var e identity
			if err := rows.Scan(&e.ID, &e.Key, &e.BusinessLine, &e.EndUser, &e.Cost); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			actual = append(actual, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].ID < expected[j].ID })
	sort.Slice(actual, func(i, j int) bool { return actual[i].ID < actual[j].ID })
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("seed %d: cross-shard usage identities differ", seed)
	}

	return dual.Stats
}
