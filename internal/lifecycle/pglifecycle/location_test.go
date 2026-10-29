package pglifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
)

func TestLookupSessionUsesIndexTarget(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	ctx := context.Background()
	id := "ses_v1_000_001_x"
	x := pglifecycle.Index{Pool: pool}
	if err := x.Put(ctx, &lifecycle.Session{ID: id, BusinessLine: "bl", EndUser: "real-user", Agent: "a", CreatedAt: time.Now(), LastInputAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	x.Select = func(target shard.Target) (*pgxpool.Pool, error) {
		calls++
		if target.Kind != shard.SessionIndex || target.Key != id {
			t.Fatalf("wrong target: %+v", target)
		}
		return pool, nil
	}
	row, err := x.LookupSession(ctx, id)
	if err != nil || row.EndUser != "real-user" || calls != 1 {
		t.Fatalf("lookup: %+v, %v, calls %d", row, err, calls)
	}
	if _, err := x.Get(ctx, "wrong-user", id); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatal("location substituted for ownership", err)
	}
	failure := errors.New("unavailable")
	x.Select = func(shard.Target) (*pgxpool.Pool, error) { return nil, failure }
	if _, err := x.LookupSession(ctx, id); !errors.Is(err, failure) {
		t.Fatal("selector failure ignored", err)
	}
	if _, err := (pglifecycle.Index{Pool: pool}).LookupSession(ctx, "ses_v9_bad"); err == nil {
		t.Fatal("direct constructor bypassed locator")
	}
}
