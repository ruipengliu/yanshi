package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/service"
	"yanshi/internal/usage"
)

type interleavedTiers struct {
	usage.TierStore
	before    func() error
	after     func() error
	deleteErr error
}

func (s *interleavedTiers) Put(ctx context.Context, bl, eu, tier string, at time.Time) error {
	if s.before != nil {
		if err := s.before(); err != nil {
			return err
		}
	}
	if err := s.TierStore.Put(ctx, bl, eu, tier, at); err != nil {
		return err
	}
	if s.after != nil {
		return s.after()
	}
	return nil
}

func (s *interleavedTiers) Delete(ctx context.Context, bl, eu string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.TierStore.Delete(ctx, bl, eu)
}

func tierCore(t *testing.T) (*app.Core, *clock.Fake, *interleavedTiers) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	stores := app.Memory(clk)
	tiers := &interleavedTiers{TierStore: stores.Tiers}
	stores.Tiers = tiers
	q := &usage.Quotas{Store: stores.Usage, Limits: map[string]usage.Limits{
		"bl": {EndUserDaily: 1, Tiers: map[string]usage.TierLimits{"pro": {EndUserDaily: 3}}},
	}}
	return app.NewCore(stores, app.Options{Clock: clk, Quotas: q}), clk, tiers
}

type contextAwareTierDeletions struct{ lifecycle.Deletions }

func (d contextAwareTierDeletions) GetEndUser(ctx context.Context, bl, eu string) (*lifecycle.EndUserTombstone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.Deletions.GetEndUser(ctx, bl, eu)
}

func TestTierEndUserDeletionInterleavings(t *testing.T) {
	for _, order := range []string{"before_write", "after_write", "cancelled_ack", "after_deletion"} {
		t.Run(order, func(t *testing.T) {
			core, _, raw := tierCore(t)
			core.Service.Deletions = contextAwareTierDeletions{Deletions: core.Stores.Deletions}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			remove := func() error {
				_, err := core.Service.DeleteEndUser(context.Background(), "bl", "u")
				return err
			}
			switch order {
			case "before_write":
				raw.before = remove
			case "after_write":
				raw.after = remove
			case "cancelled_ack":
				raw.before = remove
				raw.after = func() error {
					cancel()
					return context.Canceled
				}
			case "after_deletion":
				if err := remove(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := core.Service.PutTier(ctx, "bl", "u", "pro"); !errors.Is(err, service.ErrNotFound) {
				t.Fatalf("PutTier = %v, want deletion barrier", err)
			}
			for _, store := range []usage.TierStore{raw.TierStore, core.Service.Tiers} {
				if tier, err := store.Get(context.Background(), "bl", "u"); err != nil || tier != "" {
					t.Fatalf("tier resurrected: %q, %v", tier, err)
				}
			}
		})
	}
}

func TestTierCleanupMustSucceedBeforeSealAndReuseStartsAtDefault(t *testing.T) {
	ctx := context.Background()
	core, clk, raw := tierCore(t)
	if _, err := core.Service.PutTier(ctx, "bl", "u", "pro"); err != nil {
		t.Fatal(err)
	}
	if tier, _, err := core.Service.Quotas.EffectiveTier(ctx, "bl", "u"); err != nil || tier != "pro" {
		t.Fatalf("cached tier = %q, %v", tier, err)
	}
	raw.deleteErr = errors.New("delete unavailable")
	if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err == nil {
		t.Fatal("deletion succeeded despite failed tier cleanup")
	}
	b, err := core.Stores.Deletions.GetEndUser(ctx, "bl", "u")
	if err != nil || b.Sealed || !b.CompletedAt.IsZero() {
		t.Fatalf("barrier prematurely sealed: %+v, %v", b, err)
	}
	raw.deleteErr = nil
	if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	j := core.Janitor("j", nil, nil, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	b, err = core.Stores.Deletions.GetEndUser(ctx, "bl", "u")
	if err != nil || !b.Sealed || b.CompletedAt.IsZero() {
		t.Fatalf("barrier not completed: %+v, %v", b, err)
	}
	if _, err := core.Service.PutTier(ctx, "bl", "u", "pro"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("grace period accepted assignment: %v", err)
	}
	clk.Advance(lifecycle.DefaultEndUserGrace)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Stores.Deletions.GetEndUser(ctx, "bl", "u"); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("expired barrier remains: %v", err)
	}
	if tier, daily, err := core.Service.Quotas.EffectiveTier(ctx, "bl", "u"); err != nil || tier != "" || daily != 1 {
		t.Fatalf("reused ID inherited tier: %q, %v, %v", tier, daily, err)
	}
	if _, err := core.Service.PutTier(ctx, "bl", "u", "pro"); err != nil {
		t.Fatalf("assignment after grace: %v", err)
	}
}

type failingTierRecheck struct {
	lifecycle.Deletions
	reads int
}

func (d *failingTierRecheck) GetEndUser(ctx context.Context, bl, eu string) (*lifecycle.EndUserTombstone, error) {
	d.reads++
	if d.reads > 1 {
		return nil, errors.New("barrier read failed")
	}
	return d.Deletions.GetEndUser(ctx, bl, eu)
}

func TestTierWithdrawsWhenBarrierCannotBeRead(t *testing.T) {
	ctx := context.Background()
	core, _, raw := tierCore(t)
	core.Service.Deletions = &failingTierRecheck{Deletions: core.Stores.Deletions}
	if _, err := core.Service.PutTier(ctx, "bl", "u", "pro"); err == nil {
		t.Fatal("uncertain barrier accepted")
	}
	if tier, err := raw.Get(ctx, "bl", "u"); err != nil || tier != "" {
		t.Fatalf("uncertain barrier retained assignment: %q, %v", tier, err)
	}
}

func TestAccountDeletionClearsTiersWithQuotasDisabled(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	stores := app.Memory(clk)
	if err := stores.Tiers.Put(ctx, "bl", "u", "removed_configuration", clk.Now()); err != nil {
		t.Fatal(err)
	}
	core := app.NewCore(stores, app.Options{Clock: clk})
	if tier, err := core.Service.Tiers.Get(ctx, "bl", "u"); err != nil || tier == "" {
		t.Fatalf("initial tier: %q, %v", tier, err)
	}
	if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []usage.TierStore{stores.Tiers, core.Service.Tiers} {
		if tier, err := s.Get(ctx, "bl", "u"); err != nil || tier != "" {
			t.Fatalf("disabled quotas retained tier: %q, %v", tier, err)
		}
	}
}
