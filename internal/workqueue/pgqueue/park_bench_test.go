package pgqueue

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/workqueue"
)

// TestMillionPark 单独运行，不进 make check：
// YANSHI_BENCH_PARK=1 YANSHI_TEST_PG='<DSN>' go test ./internal/workqueue/pgqueue -run '^TestMillionPark$' -count=1 -timeout=20m -v
// Fresh 创建隔离 schema 并在结束时删除；批量数据沿用一次真实 Park 生成的类别、期限与列形状，
// 用 generate_series 准备，避免百万次客户端往返支配压测时间。ord 各自取序列，保持真实顺序形状。
func TestMillionPark(t *testing.T) {
	if os.Getenv("YANSHI_BENCH_PARK") != "1" {
		t.Skip("YANSHI_BENCH_PARK not set")
	}
	ctx := context.Background()
	pool, _ := pgtest.Fresh(t)
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := New(pool, c, Sessions)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		must(err)
	}
	class := workqueue.Class{BusinessLine: "bench", Priority: workqueue.Interactive}
	until := c.Now().Add(24 * time.Hour)
	// 真实 Park 模板在所有活跃项之前入队；对照场景中停放项排在活跃项之前，不能被新 ord 藏到扫描尾部。
	must(q.Enqueue(ctx, "template", class))
	l, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{})
	must(err)
	must(q.Park(ctx, l, until))
	// schema 内测试表供连接池共用，Fresh 一并清理。
	exec(`CREATE TABLE bench_template AS SELECT * FROM work_dues`)
	must(q.Remove(ctx, "template"))
	prepareReady := func() {
		exec(`DELETE FROM work_items WHERE session_id LIKE 'ready-%'`)
		exec(`INSERT INTO work_items (session_id, business_line, priority, ord, ready_at)
			SELECT 'ready-' || i, 'bench', 2, nextval('work_seq'), $1 FROM generate_series(1, 10000) i`, c.Now())
		exec(`ANALYZE work_items`)
	}
	type latency struct{ p50, p99 time.Duration }
	measure := func() latency {
		samples := make([]time.Duration, 1000)
		for i := range 1100 {
			start := time.Now()
			l, err := q.Claim(ctx, "bench", time.Minute, workqueue.Pool{})
			must(err)
			must(q.Release(ctx, l, false))
			if i >= 100 {
				samples[i-100] = time.Since(start)
			}
		}
		slices.Sort(samples)
		return latency{samples[len(samples)/2], samples[(len(samples)*99+99)/100-1]}
	}
	// 已存在且未停放的同一项，单独量化入队事务锁与到期表查找；只报告，不设门槛。
	measureEnqueue := func() latency {
		samples := make([]time.Duration, 1000)
		for i := range 1100 {
			start := time.Now()
			must(q.Enqueue(ctx, "ready-1", class))
			if i >= 100 {
				samples[i-100] = time.Since(start)
			}
		}
		slices.Sort(samples)
		return latency{samples[len(samples)/2], samples[(len(samples)*99+99)/100-1]}
	}
	prepareReady()
	base := measure()
	enqueueBase := measureEnqueue()
	// 将原来的活跃项重建在百万停放之后，使两个百万场景的 ord 分布一致。
	exec(`DELETE FROM work_items`)
	exec(`INSERT INTO work_dues (session_id, business_line, priority, ord, due_at)
		SELECT 'park-' || i, t.business_line, t.priority, nextval('work_seq'), t.due_at
		FROM bench_template t CROSS JOIN generate_series(1, 1000000) i`)
	exec(`ANALYZE work_dues`)
	prepareReady()
	deferred := measure()
	enqueueDeferred := measureEnqueue()
	exec(`INSERT INTO work_items (session_id, business_line, priority, ord, parked_until, ready_at)
		SELECT session_id, business_line, priority, ord, due_at, due_at FROM work_dues`)
	exec(`DELETE FROM work_dues`)
	prepareReady()
	inline := measure()
	exec(`TRUNCATE work_items, work_dues`)
	exec(`INSERT INTO work_dues (session_id, business_line, priority, ord, due_at)
		SELECT 'due-' || i, t.business_line, t.priority, nextval('work_seq'), t.due_at
		FROM bench_template t CROSS JOIN generate_series(1, 10000) i`)
	exec(`ANALYZE work_dues`)
	start := time.Now()
	n, err := q.Promote(ctx, until, 10000)
	elapsed := time.Since(start)
	must(err)
	throughput := float64(n) / elapsed.Seconds()
	t.Logf("\n| 场景 | 操作 | p50 | p99 | Promote 总耗时 | 吞吐（项/秒） |\n|---|---|---:|---:|---:|---:|\n| 1 万活跃基线 | Claim + Release | %s | %s | — | — |\n| 加百万到期表停放 | Claim + Release | %s | %s | — | — |\n| 百万 work_items 停放对照 | Claim + Release | %s | %s | — | — |\n| 迁移 1 万项 | Promote | — | — | %s | %.0f |\n| 1 万活跃、无远期 | Enqueue | %s | %s | — | — |\n| 1 万活跃、百万远期 | Enqueue | %s | %s | — | — |", base.p50, base.p99, deferred.p50, deferred.p99, inline.p50, inline.p99, elapsed, throughput, enqueueBase.p50, enqueueBase.p99, enqueueDeferred.p50, enqueueDeferred.p99)
	if deferred.p99 > 2*base.p99 {
		t.Errorf("deferred p99 %s exceeds 2x baseline %s", deferred.p99, base.p99)
	}
	if n != 10000 {
		t.Errorf("promoted %d, want 10000", n)
	}
	if throughput < 5000 {
		t.Errorf("promote throughput %.0f items/s below 5000 (%d items in %s)", throughput, n, elapsed)
	}
	var count int
	must(pool.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE parked_until = $1 AND ready_at = $1`, until).Scan(&count))
	if count != 10000 {
		t.Errorf("restored deadlines: %d", count)
	}
}
