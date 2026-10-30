package pg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"xinli/pg"
	"xinli/pg/pgtest"
)

func TestAuthenticationAcquireHasSeparateBudget(t *testing.T) {
	_, dsn := pgtest.NewWithDSN(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	entered := false
	started := time.Now()
	err = pg.WithConnection(ctx, pool, func(pg.DB) error { entered = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || entered || time.Since(started) >= 2*time.Second {
		t.Fatalf("borrow err=%v entered=%t elapsed=%s", err, entered, time.Since(started))
	}
	held.Release()
	if err := pg.WithConnection(ctx, pool, func(db pg.DB) error { var one int; return db.QueryRow(ctx, `SELECT 1`).Scan(&one) }); err != nil {
		t.Fatal(err)
	}
}
