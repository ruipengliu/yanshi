package janitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/pg/pgtest"
)

func pgTierCleanupCore(t *testing.T, maxConns int32) (*app.Core, *clock.Fake) {
	t.Helper()
	pool, notifier := pgtest.Fresh(t)
	cfg := pool.Config()
	cfg.MaxConns = maxConns
	limited, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(limited.Close)
	clk := clock.NewFake(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	return app.NewCore(app.Postgres(limited, notifier, clk), app.Options{Clock: clk, EndUserGrace: time.Hour}), clk
}

func TestExpiredEndUserBarrierPGCleanupSharesTransaction(t *testing.T) {
	// 迁移与装配受并行测试负载影响，不应占用被测操作的期限；单连接自锁仍须在十秒内失败。
	core, clk := pgTierCleanupCore(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	j := core.Janitor("j", nil, nil, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, bl := range []string{"bl", "other"} {
		if err := core.Stores.Tiers.Put(ctx, bl, "u", "pro", clk.Now()); err != nil {
			t.Fatal(err)
		}
	}
	failed := errors.New("another holder failed")
	fail := true
	j.EndUserData = append(j.EndUserData, lifecycle.EndUserHolder{Name: "failure", Delete: func(context.Context, string, string) error {
		if fail {
			return failed
		}
		return nil
	}})
	clk.Advance(time.Hour)
	// 只有一个连接：回调若另借连接而不沿用逐 EndUser 事务，这里会超时。
	if err := j.Sweep(ctx); !errors.Is(err, failed) {
		t.Fatalf("cleanup failure = %v", err)
	}
	if tier, err := core.Stores.Tiers.Get(ctx, "bl", "u"); err != nil || tier != "pro" {
		t.Fatalf("failed cleanup did not roll back tier deletion: %q, %v", tier, err)
	}
	if _, err := core.Stores.Deletions.GetEndUser(ctx, "bl", "u"); err != nil {
		t.Fatalf("failed cleanup removed barrier: %v", err)
	}
	fail = false
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if tier, err := core.Stores.Tiers.Get(ctx, "bl", "u"); err != nil || tier != "" {
		t.Fatalf("retry left tier: %q, %v", tier, err)
	}
	if _, err := core.Stores.Deletions.GetEndUser(ctx, "bl", "u"); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("retry retained barrier: %v", err)
	}
	if tier, err := core.Stores.Tiers.Get(ctx, "other", "u"); err != nil || tier != "pro" {
		t.Fatalf("cleanup crossed business line: %q, %v", tier, err)
	}
}

func TestExpiredEndUserBarrierPGInvalidatesCacheAfterCommit(t *testing.T) {
	core, clk := pgTierCleanupCore(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	j := core.Janitor("j", nil, nil, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := core.Stores.Tiers.Put(ctx, "bl", "u", "pro", clk.Now()); err != nil {
		t.Fatal(err)
	}
	reads := 0
	j.EndUserData = append(j.EndUserData, lifecycle.EndUserHolder{Name: "concurrent_read", Delete: func(_ context.Context, bl, eu string) error {
		// 另一连接只看已提交数据；故意在清理事务提交前回填旧档位。
		tier, err := core.Service.Tiers.Get(ctx, bl, eu)
		if err != nil {
			return err
		}
		if tier != "pro" {
			t.Fatalf("pre-commit read = %q, want old tier", tier)
		}
		reads++
		return nil
	}})
	clk.Advance(time.Hour)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if tier, err := core.Service.Tiers.Get(ctx, "bl", "u"); err != nil || tier != "" || reads != 1 {
		t.Fatalf("cache retained pre-commit tier: %q, %v, reads %d", tier, err, reads)
	}
}
