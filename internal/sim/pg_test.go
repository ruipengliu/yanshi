package sim

import (
	"flag"
	"fmt"
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/memory/pgmemory"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/notify/pgnotify"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/presence/pgpresence"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/session/pgsnapshot"
	"yanshi/internal/usage/pgusage"
	"yanshi/internal/workqueue/pgqueue"
)

var pgSeeds = flag.Int("sim.pgseeds", 15, "number of seeds to simulate on PostgreSQL")

// TestPostgresMatchesMemory 是差分测试：同一种子在 PostgreSQL 与内存实现上必须产生逐字节相同的日志，
// 从而证明两套存储实现语义等价，内存模拟的全部结论同样适用于 PostgreSQL。
func TestPostgresMatchesMemory(t *testing.T) {
	pgtest.DSN(t)
	for s := uint64(1); s <= uint64(*pgSeeds); s++ {
		// 每个种子一个子测试，使其数据库 schema 与连接池在子测试结束时即释放。
		t.Run(fmt.Sprintf("seed%d", s), func(t *testing.T) { diffSeed(t, s) })
	}
}

func diffSeed(t *testing.T, s uint64) {
	opts := Options{Seed: s, Workers: 3, Sessions: 3, Ticks: 300, Faults: true}
	if s%3 == 0 { // 部分种子跑长 Run，覆盖压缩事件在 PostgreSQL 上的往返
		opts.LongRuns, opts.Sessions, opts.Ticks = true, 2, 800
	}
	mem, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Run(); err != nil {
		t.Fatalf("seed %d (memory): %v", s, err)
	}
	if mem.Stats.Runs == 0 {
		t.Fatalf("seed %d: simulation did nothing", s)
	}

	pool, notifier := pgtest.Fresh(t)
	opts.NewStores = func(c clock.Clock) Stores {
		return Stores{
			Log: pglog.New(pool, notifier), Queue: pgqueue.New(pool, c, pgqueue.Sessions),
			Dir: pgnode.NewDirectory(pool, c), Inbox: pgnode.NewInbox(pool, notifier),
			SandboxQueue: pgqueue.New(pool, c, pgqueue.Sandboxes),
			Ledger:       pgsandbox.Ledger{Pool: pool}, Activity: pgsandbox.Activity{Pool: pool},
			Index: pglifecycle.Index{Pool: pool}, Deletions: pglifecycle.Deletions{Pool: pool, Notifier: notifier},
			JanitorQueue: pgqueue.New(pool, c, pgqueue.Janitor),
			Memory:       pgmemory.Store{Pool: pool}, Grants: pgmemory.Grants{Pool: pool},
			Snapshots: pgsnapshot.Store{Pool: pool}, Usage: pgusage.Store{Pool: pool},
			Presence: pgpresence.Store{Pool: pool, Notifier: notifier}, Push: pgnotify.Registry{Pool: pool},
		}
	}
	onPG, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := onPG.Run(); err != nil {
		t.Fatalf("seed %d (postgres): %v", s, err)
	}
	if mem.Fingerprint() != onPG.Fingerprint() {
		t.Fatalf("seed %d: postgres and memory runs diverged\nreproduce: go test ./internal/sim -run 'TestPostgresMatchesMemory/seed%d$' -sim.pgseeds=%d", s, s, s)
	}
}
