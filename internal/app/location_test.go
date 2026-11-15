package app_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/app"
	"yanshi/internal/shard"
)

func TestSinglePoolSelector(t *testing.T) {
	pool := new(pgxpool.Pool)
	selectPool := app.SinglePoolSelector(pool, shard.DefaultLocator())
	if got, err := selectPool(shard.Target{Kind: shard.SessionIndex, Key: "ses_v1_000_000_x"}); err != nil || got != pool {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"ses_v9_bad", "ses_old"} {
		if _, err := selectPool(shard.Target{Kind: shard.SessionIndex, Key: bad}); err == nil {
			t.Fatalf("locator bypassed for %q", bad)
		}
	}
	table, _ := shard.NewTable(shard.Config{Shards: []int{0, 1}, Ranges: []shard.Range{{First: 0, Last: 1023, Shard: 1}}})
	loc, err := shard.NewLocator(table, "new-cell")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.SinglePoolSelector(pool, loc)(shard.Target{Kind: shard.Object, Key: "art_v1_000_x"}); err == nil {
		t.Fatal("unassembled shard silently fell back")
	}
}
