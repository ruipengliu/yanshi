package sim

import (
	"flag"
	"fmt"
	"testing"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/pg/pgtest"
)

var pgSeeds = flag.Int("sim.pgseeds", 15, "number of seeds to simulate on PostgreSQL")

// pgSlots 限制同时连接 PostgreSQL 的种子数。每个种子有自己的 schema、连接池与 Notifier，实测并发时每个种子约占
// 4 条连接；开发数据库的 max_connections 为 100，且由各 worktree 与 make check 中其他用到 PostgreSQL 的包共用。
// 种子的模拟主要在等数据库往返，并行能缩短墙钟时间；限为 8 个（约 32 条连接），核数再多也不越过这个余量。
var pgSlots = make(chan struct{}, 8)

// TestPostgresMatchesMemory 是差分测试：同一种子在 PostgreSQL 与内存实现上必须产生逐字节相同的日志，
// 从而证明两套存储实现语义等价，内存模拟的全部结论同样适用于 PostgreSQL。
func TestPostgresMatchesMemory(t *testing.T) {
	pgtest.DSN(t)
	for s := uint64(1); s <= uint64(*pgSeeds); s++ {
		// 每个种子一个并行的子测试，使其数据库 schema 与连接池在子测试结束时即释放。
		t.Run(fmt.Sprintf("seed%d", s), func(t *testing.T) {
			t.Parallel()
			diffSeed(t, s)
		})
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

	// 内存一侧不占连接，不受限；先登记释放，使其在 Fresh 关闭连接池之后执行（Cleanup 后进先出）。
	pgSlots <- struct{}{}
	t.Cleanup(func() { <-pgSlots })
	pool, notifier := pgtest.Fresh(t)
	opts.NewStores = func(c clock.Clock) Stores { return app.Postgres(pool, notifier, c) }
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
