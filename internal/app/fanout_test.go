package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/shard"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

type countedUsage struct {
	usage.Store
	value  int64
	calls  atomic.Int32
	failed atomic.Bool
}

func TestFanoutPreservesCallbackAndStorageErrors(t *testing.T) {
	failed := errors.New("cleanup failed")
	for _, original := range []error{failed, pgx.ErrNoRows, &pgconn.PgError{Code: "23505"}, context.Canceled} {
		for _, complete := range []bool{false, true} {
			r := fanoutRouter(t)
			parts, err := collect(context.Background(), r, "callback", complete, func(context.Context, Stores) (int, error) {
				return 0, fmt.Errorf("callback: %w", original)
			})
			if len(parts) != 0 || !errors.Is(err, original) || errors.Is(err, shard.ErrUnavailable) {
				t.Fatalf("callback error replaced (complete=%t): %v", complete, err)
			}
		}
	}
	// 单片成功不能把另一片的业务错误当成可忽略的失联；健康片仍然交回候选。
	r := fanoutRouter(t)
	r.stores[0] = Stores{Usage: &countedUsage{}}
	values, err := collect(context.Background(), r, "partial", false, func(_ context.Context, s Stores) (int, error) {
		if s.Usage != nil {
			return 0, failed
		}
		return 7, nil
	})
	if len(values) != 1 || values[0] != 7 || !errors.Is(err, failed) || errors.Is(err, shard.ErrUnavailable) {
		t.Fatalf("partial success hid callback error: %v %v", values, err)
	}
	r.available = func(context.Context, int) error { return failed }
	if _, err := collect(context.Background(), r, "guard", true, func(context.Context, Stores) (int, error) {
		t.Error("callback ran after failed guard")
		return 0, nil
	}); !errors.Is(err, failed) || errors.Is(err, shard.ErrUnavailable) {
		t.Fatalf("guard error replaced: %v", err)
	}
}

func TestFanoutUnavailableConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), "postgres://user:secret@"+address+"/unavailable?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	r := fanoutRouter(t)
	_, err = collect(context.Background(), r, "connection", true, func(ctx context.Context, _ Stores) (int, error) {
		return 0, pool.Ping(ctx)
	})
	if !errors.Is(err, shard.ErrUnavailable) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), address) {
		t.Fatalf("connection failure not safely classified: %v", err)
	}
}

func TestFanoutMixedErrorsPreserveBusinessFailure(t *testing.T) {
	failed := errors.New("cleanup failed before timeout")
	for _, transport := range []error{context.DeadlineExceeded, io.EOF, shard.ErrUnavailable, &net.OpError{
		Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321}, Err: errors.New("refused"),
	}} {
		original := fmt.Errorf("cleanup: %w", errors.Join(failed, transport))
		for _, complete := range []bool{false, true} {
			r := fanoutRouter(t)
			r.stores[0] = Stores{Usage: &countedUsage{}}
			values, err := collect(context.Background(), r, "joined", complete, func(_ context.Context, s Stores) (int, error) {
				if s.Usage != nil {
					return 0, original
				}
				return 7, nil
			})
			if !errors.Is(err, failed) || !errors.Is(err, original) || onlyUnavailable(err) {
				t.Fatalf("mixed error replaced (complete=%t): %v", complete, err)
			}
			if strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("mixed error leaked connection address: %v", err)
			}
			if !complete && (len(values) != 1 || values[0] != 7) {
				t.Fatalf("healthy candidates lost: %v", values)
			}
		}
	}
}

func TestFanoutExhaustedBudgetIsUnavailable(t *testing.T) {
	r := fanoutRouter(t)
	r.fanout = FanoutOptions{Concurrency: 1, ShardTimeout: time.Second, Budget: 10 * time.Millisecond}
	parts := fanout(context.Background(), r, "budget", func(ctx context.Context, _ Stores, _ string) (int, string, error) {
		<-ctx.Done()
		return 0, "", ctx.Err()
	})
	for _, part := range parts {
		if !errors.Is(part.Err, shard.ErrUnavailable) {
			t.Fatalf("exhausted budget misclassified: %v", part.Err)
		}
	}
}

func TestFanoutIndependentScanPreservesError(t *testing.T) {
	r := fanoutRouter(t)
	r.stores[0] = Stores{Usage: &countedUsage{}}
	failed := errors.New("index scan failed")
	rows, err := r.scanIDs(context.Background(), "scan-error", 1, func(_ context.Context, s Stores) ([]string, error) {
		if s.Usage != nil {
			return nil, failed
		}
		return []string{"healthy"}, nil
	})
	if len(rows) != 1 || rows[0] != "healthy" || !errors.Is(err, failed) || errors.Is(err, shard.ErrUnavailable) {
		t.Fatalf("independent scan hid storage error: %v %v", rows, err)
	}
}

type failedClaimQueue struct {
	workqueue.Queue
	err error
}

func (q failedClaimQueue) Claim(context.Context, string, time.Duration, workqueue.Pool) (*workqueue.Lease, error) {
	return nil, q.err
}

func TestShardedClaimPreservesStorageError(t *testing.T) {
	failed := errors.New("claim rejected")
	for _, original := range []error{failed, errors.Join(failed, shard.ErrUnavailable)} {
		r := fanoutRouter(t)
		r.stores[0] = Stores{Queue: failedClaimQueue{err: original}}
		r.stores[1] = Stores{Queue: failedClaimQueue{err: workqueue.ErrEmpty}}
		q := &routedQueue{r: r, get: func(s Stores) workqueue.Queue { return s.Queue }, timeout: time.Second}
		if _, err := q.Claim(context.Background(), "worker", time.Minute, workqueue.Pool{}); !errors.Is(err, failed) || !errors.Is(err, original) || onlyUnavailable(err) {
			t.Fatalf("claim error replaced: %v", err)
		}
		r.available = func(context.Context, int) error { return original }
		if _, err := q.Claim(context.Background(), "worker", time.Minute, workqueue.Pool{}); !errors.Is(err, failed) || !errors.Is(err, original) || onlyUnavailable(err) {
			t.Fatalf("claim guard error replaced: %v", err)
		}
	}
}

func (s *countedUsage) Spent(ctx context.Context, _, _ string, _, _ time.Time) (int64, error) {
	s.calls.Add(1)
	if s.failed.Load() {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.value, nil
}

func fanoutRouter(t *testing.T) *storeRouter {
	t.Helper()
	l, err := shard.ParseDeployment([]byte(`{"version":"test","table":{"shards":[0,1],"ranges":[{"first":0,"last":511,"shard":0},{"first":512,"last":1023,"shard":1}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return &storeRouter{locator: l, ids: l.Shards(), stores: map[int]Stores{0: {}, 1: {}}, positions: map[string]*scanPosition{}, fanout: FanoutOptions{Concurrency: 2, ShardTimeout: 20 * time.Millisecond, Budget: 60 * time.Millisecond}}
}

func TestFanoutBoundsAndIndependentProgress(t *testing.T) {
	r := fanoutRouter(t)
	var active, peak atomic.Int32
	start := time.Now()
	_, err := collect(context.Background(), r, "blocked", true, func(ctx context.Context, _ Stores) (int, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, shard.ErrUnavailable) || peak.Load() > 2 || active.Load() != 0 || time.Since(start) > time.Second {
		t.Fatalf("fanout bounds: %v peak=%d", err, peak.Load())
	}
	r.available = func(_ context.Context, id int) error {
		if id == 0 {
			return shard.ErrUnavailable
		}
		return nil
	}
	values, err := collect(context.Background(), r, "independent", false, func(context.Context, Stores) (int, error) { return 7, nil })
	if err != nil || len(values) != 1 || values[0] != 7 {
		t.Fatalf("healthy work discarded: %v %v", values, err)
	}
	if _, err := collect(context.Background(), r, "complete", true, func(context.Context, Stores) (int, error) { return 7, nil }); !errors.Is(err, shard.ErrUnavailable) {
		t.Fatal("partial result accepted")
	}
}

func TestSpentCompleteCache(t *testing.T) {
	r := fanoutRouter(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.usageCache = newSpentCache(ShardOptions{Now: func() time.Time { return now }, UsageCacheTTL: time.Minute})
	a, b := &countedUsage{value: 3}, &countedUsage{value: 5}
	r.stores[0], r.stores[1] = Stores{Usage: a}, Stores{Usage: b}
	from, to := now, now.Add(time.Hour)
	read := func(line string) (int64, error) { return r.spent(context.Background(), line, "", from, to) }
	if got, err := read("bl"); err != nil || got != 8 {
		t.Fatalf("complete: %d %v", got, err)
	}
	b.failed.Store(true)
	if got, err := read("bl"); err != nil || got != 8 {
		t.Fatalf("valid cache: %d %v", got, err)
	}
	if _, err := read("uncached"); !errors.Is(err, shard.ErrUnavailable) {
		t.Fatal("missing usage treated as zero")
	}
	now = now.Add(time.Minute)
	if _, err := read("bl"); !errors.Is(err, shard.ErrUnavailable) {
		t.Fatal("expired cache accepted")
	}
	b.failed.Store(false)
	if _, err := read("bl"); err != nil {
		t.Fatal(err)
	}
	// 标签相同但映射不同也失效；摘要属于缓存键，不能仅比较版本标签。
	changed, err := shard.ParseDeployment([]byte(`{"version":"test","table":{"shards":[0,1],"ranges":[{"first":0,"last":255,"shard":0},{"first":256,"last":1023,"shard":1}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	r.locator = changed
	b.failed.Store(true)
	if _, err := read("bl"); !errors.Is(err, shard.ErrUnavailable) {
		t.Fatal("changed identity reused cache")
	}
}

func TestFanoutCursorRetriesAndWraps(t *testing.T) {
	r := fanoutRouter(t)
	for round := range 3 {
		parts := fanout(context.Background(), r, "cursor", func(_ context.Context, _ Stores, cursor string) (string, string, error) {
			if round == 1 {
				return cursor, "wrong", errors.New("retry")
			}
			return cursor, "next", nil
		})
		for _, part := range parts {
			want := ""
			if round > 0 {
				want = "next"
			}
			if part.Value != want {
				t.Fatalf("cursor advanced on failure: %q want %q", part.Value, want)
			}
		}
	}
}

func TestDeterministicFanoutUsesStableOrder(t *testing.T) {
	r := fanoutRouter(t)
	r.deterministic = true
	var calls []int
	r.available = func(_ context.Context, id int) error { calls = append(calls, id); return nil }
	for range 3 {
		parts := fanout(context.Background(), r, "sim", func(ctx context.Context, _ Stores, cursor string) (string, string, error) {
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				t.Fatal("simulation uses wall clock deadline")
			}
			return cursor, "next", nil
		})
		if parts[0].ID != 0 || parts[1].ID != 1 {
			t.Fatal("simulation order changed")
		}
	}
	for i, id := range calls {
		if id != i%2 {
			t.Fatal("simulation interleaving changed")
		}
	}
}

// 上线按 8 个分片部署（ADR-0051 §5.4，2026-11-18）：并发上限 4 时一次扇出分两波。
// 第一波中失联的分片只占用各自的时限，第二波的健康分片照常交回结果；起点逐轮轮转。
func TestFanoutEightShardsSpansTwoWaves(t *testing.T) {
	l, err := shard.ParseDeployment([]byte(`{"version":"test","table":{"shards":[0,1,2,3,4,5,6,7],"ranges":[` +
		`{"first":0,"last":127,"shard":0},{"first":128,"last":255,"shard":1},{"first":256,"last":383,"shard":2},{"first":384,"last":511,"shard":3},` +
		`{"first":512,"last":639,"shard":4},{"first":640,"last":767,"shard":5},{"first":768,"last":895,"shard":6},{"first":896,"last":1023,"shard":7}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	stores := map[int]Stores{}
	for id := range 8 {
		u := &countedUsage{value: int64(id)}
		u.failed.Store(id%2 == 0) // 四个失联分片分散在两波中
		stores[id] = Stores{Usage: u}
	}
	r := &storeRouter{locator: l, ids: l.Shards(), stores: stores, positions: map[string]*scanPosition{},
		fanout: FanoutOptions{Concurrency: 4, ShardTimeout: 20 * time.Millisecond, Budget: time.Second}}
	var active, peak atomic.Int32
	firsts := map[int]bool{}
	for range 3 {
		start := time.Now()
		parts := fanout(context.Background(), r, "eight", func(ctx context.Context, s Stores, cursor string) (int64, string, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			v, err := s.Usage.Spent(ctx, "", "", time.Time{}, time.Time{})
			return v, cursor, err
		})
		firsts[parts[0].ID] = true
		healthy := 0
		for _, part := range parts {
			switch {
			case part.ID%2 == 0 && !errors.Is(part.Err, shard.ErrUnavailable):
				t.Fatalf("hung shard %d not unavailable: %v", part.ID, part.Err)
			case part.ID%2 == 1 && (part.Err != nil || part.Value != int64(part.ID)):
				t.Fatalf("healthy shard %d lost: %v %v", part.ID, part.Value, part.Err)
			case part.ID%2 == 1:
				healthy++
			}
		}
		if len(parts) != 8 || healthy != 4 {
			t.Fatalf("parts=%d healthy=%d", len(parts), healthy)
		}
		// 两波各至多一个分片时限，远小于整轮预算。
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("eight-shard fanout took %v", elapsed)
		}
	}
	if peak.Load() > 4 || active.Load() != 0 {
		t.Fatalf("concurrency peak=%d active=%d", peak.Load(), active.Load())
	}
	if len(firsts) < 2 {
		t.Fatalf("start position did not rotate: %v", firsts)
	}
}
