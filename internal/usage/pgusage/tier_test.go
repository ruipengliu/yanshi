package pgusage_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/usage"
	"yanshi/internal/usage/pgusage"
	"yanshi/internal/usage/tiertest"
)

func TestTiers(t *testing.T) {
	tiertest.Run(t, func(t *testing.T) usage.TierStore {
		pool, _ := pgtest.Fresh(t)
		return pgusage.Tiers{Pool: pool}
	})
}

func TestTiersPutSharesTransaction(t *testing.T) {
	for _, previous := range []string{"", "basic"} {
		name := "insert"
		if previous != "" {
			name = "update"
		}
		t.Run(name, func(t *testing.T) {
			pool, _ := pgtest.Fresh(t)
			cfg := pool.Config()
			cfg.MaxConns = 1
			limited, err := pgxpool.NewWithConfig(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(limited.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store := pgusage.Tiers{Pool: limited}
			if previous != "" {
				if err := store.Put(ctx, "bl", "u", previous, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := limited.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			// 唯一连接已经属于事务；另借连接会超时，独立提交也不能满足回滚断言。
			if err := store.Put(pg.WithTx(ctx, limited, tx), "bl", "u", "pro", time.Now()); err != nil {
				t.Fatal(err)
			}
			var pending string
			if err := tx.QueryRow(ctx, `SELECT tier FROM end_user_tiers WHERE business_line=$1 AND end_user=$2`, "bl", "u").Scan(&pending); err != nil || pending != "pro" {
				t.Fatalf("transaction write = %q, %v", pending, err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if got, err := store.Get(ctx, "bl", "u"); err != nil || got != previous {
				t.Fatalf("rollback restored %q, want %q: %v", got, previous, err)
			}
		})
	}
}
