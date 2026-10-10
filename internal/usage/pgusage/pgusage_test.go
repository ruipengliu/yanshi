package pgusage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"yanshi/internal/pg/pgtest"
	"yanshi/internal/usage"
	"yanshi/internal/usage/pgusage"
	"yanshi/internal/usage/usagetest"
)

func TestStore(t *testing.T) {
	usagetest.Run(t, func(t *testing.T) usage.Store {
		pool, _ := pgtest.Fresh(t)
		return pgusage.Store{Pool: pool}
	})
}

// TestTotalsAreShardedAndSum：业务线合计分散在多个分片行上，Spent 求和后与逐条累加一致；EndUser 行不分片。
func TestTotalsAreShardedAndSum(t *testing.T) {
	ctx := context.Background()
	pool, _ := pgtest.Fresh(t)
	s := pgusage.Store{Pool: pool}
	at := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	var want int64
	for i := range 200 {
		cost := int64(i%7 + 1)
		want += cost
		if err := s.Record(ctx, &usage.Entry{ID: fmt.Sprintf("e%d", i), BusinessLine: "a", EndUser: fmt.Sprintf("u%d", i%3), Kind: usage.Model, Cost: cost, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	var shards, userShards int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT shard) FROM usage_hourly WHERE end_user = ''`).Scan(&shards); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_hourly WHERE end_user <> '' AND shard <> 0`).Scan(&userShards); err != nil {
		t.Fatal(err)
	}
	if shards < 8 || userShards != 0 {
		t.Fatalf("total spread over %d shards (want most of 16); %d end-user rows off shard 0", shards, userShards)
	}
	hour := at.Truncate(time.Hour)
	if got, _ := s.Spent(ctx, "a", "", hour, hour.Add(time.Hour)); got != want {
		t.Fatalf("business line spent %d, want %d", got, want)
	}
	_ = s.DeleteEndUser(ctx, "a", "u1")
	_ = s.AnonymizeEntry(ctx, "e0")
	if got, _ := s.Spent(ctx, "a", "", hour, hour.Add(time.Hour)); got != want {
		t.Fatalf("total changed by anonymization: %d, want %d", got, want)
	}
}
