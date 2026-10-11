package usage_test

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/usage"
)

// countingStore 统计 Spent 的查询次数。
type countingStore struct {
	usage.Store
	queries int
}

func (s *countingStore) Spent(ctx context.Context, bl, eu string, from, to time.Time) (int64, error) {
	s.queries++
	return s.Store.Spent(ctx, bl, eu, from, to)
}

func TestSpentCache(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC))
	inner := &countingStore{Store: usage.NewMem()}
	c := usage.NewSpentCache(inner, time.Minute, clk)
	from, to := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	record := func(id, eu string, cost int64) {
		t.Helper()
		if err := c.Record(ctx, &usage.Entry{ID: id, BusinessLine: "bl", EndUser: eu, Kind: usage.Model, Model: "m", Cost: cost, At: clk.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	spent := func(eu string) int64 {
		t.Helper()
		v, err := c.Spent(ctx, "bl", eu, from, to)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	record("e1", "u1", 100)
	if spent("u1") != 100 || spent("") != 100 || inner.queries != 2 {
		t.Fatalf("first reads: queries %d", inner.queries)
	}
	// 经缓存写入的用量立即计入缓存，不再查询。
	record("e2", "u1", 50)
	record("e3", "u2", 7)
	if got := spent("u1"); got != 150 {
		t.Fatalf("u1 spent %d, want 150", got)
	}
	if got := spent(""); got != 157 {
		t.Fatalf("business line spent %d, want 157", got)
	}
	if inner.queries != 2 {
		t.Fatalf("queried %d times within the TTL", inner.queries)
	}
	// 绕过缓存的写入（其他进程）在 TTL 之后可见。
	if err := inner.Record(ctx, &usage.Entry{ID: "e4", BusinessLine: "bl", EndUser: "u1", Kind: usage.Model, Model: "m", Cost: 1000, At: clk.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := spent("u1"); got != 150 {
		t.Fatalf("cached u1 spent %d", got)
	}
	clk.Advance(time.Minute)
	if got := spent("u1"); got != 1150 || inner.queries != 3 {
		t.Fatalf("after TTL: spent %d, queries %d", got, inner.queries)
	}
}
