package usage_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/usage"
	"yanshi/internal/usage/tiertest"
)

func TestMemTiers(t *testing.T) {
	tiertest.Run(t, func(*testing.T) usage.TierStore { return usage.NewMemTiers() })
}

func TestTierLimits(t *testing.T) {
	for _, name := range []string{"a", "pro_2-", strings.Repeat("a", 32)} {
		l := usage.Limits{Tiers: map[string]usage.TierLimits{name: {EndUserDaily: 1}}}
		if err := l.Prepare(); err != nil || !l.Enabled() {
			t.Fatalf("tier-only limits: %v, enabled %v", err, l.Enabled())
		}
	}
	for _, name := range []string{"", "PRO", "a.b", "会员", strings.Repeat("a", 33)} {
		l := usage.Limits{Tiers: map[string]usage.TierLimits{name: {EndUserDaily: 1}}}
		if err := l.Prepare(); err == nil {
			t.Errorf("accepted tier name %q", name)
		}
	}
	for _, daily := range []float64{0, -1, math.NaN(), math.Inf(1), 0.0000001} {
		l := usage.Limits{Tiers: map[string]usage.TierLimits{"pro": {EndUserDaily: daily}}}
		if err := l.Prepare(); err == nil {
			t.Errorf("accepted tier limit %v", daily)
		}
	}
}

func TestQuotaTierResolution(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s, tiers := usage.NewMem(), usage.NewMemTiers()
	l := usage.Limits{Monthly: 10, EndUserDaily: 1, Tiers: map[string]usage.TierLimits{"pro": {EndUserDaily: 3}}}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	q := &usage.Quotas{Store: s, Tiers: tiers, Limits: map[string]usage.Limits{"bl": l}}
	if err := s.Record(ctx, &usage.Entry{ID: "e", BusinessLine: "bl", EndUser: "u", Kind: usage.Model, Cost: usage.Yuan(2), At: now}); err != nil {
		t.Fatal(err)
	}
	before, err := q.Status(ctx, "bl", "u", now)
	if err != nil || len(before) != 2 || !before[1].Exceeded {
		t.Fatalf("default periods: %+v, %v", before, err)
	}
	if err := tiers.Put(ctx, "bl", "u", "pro", now); err != nil {
		t.Fatal(err)
	}
	after, err := q.Status(ctx, "bl", "u", now)
	if err != nil || len(after) != 2 || after[1].Tier != "pro" || after[1].Limit != usage.Yuan(3) || after[1].Exceeded {
		t.Fatalf("tier periods: %+v, %v", after, err)
	}
	if *before[0] != *after[0] || before[1].Spent != after[1].Spent || before[1].Start != after[1].Start || before[1].ResetAt != after[1].ResetAt {
		t.Fatal("tier changed monthly quota, usage or period")
	}
	if p, err := q.Check(ctx, "bl", "u", now); err != nil || p != nil {
		t.Fatalf("upgrade did not admit: %+v, %v", p, err)
	}
	if periods, err := q.Status(ctx, "bl", "", now); err != nil || len(periods) != 1 {
		t.Fatalf("business line status: %+v, %v", periods, err)
	}
	if p, err := q.Check(ctx, "bl", "other", now); err != nil || p != nil {
		t.Fatalf("isolated user: %+v, %v", p, err)
	}
	// 移除配置后回落默认；计数不带档位、业务线或 EndUser 标签。
	metricValue := func() float64 {
		t.Helper()
		families, err := metrics.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() == "yanshi_quota_unknown_tier_total" {
				m := family.GetMetric()[0]
				if len(m.GetLabel()) != 0 {
					t.Fatal("unknown tier metric has labels")
				}
				return m.GetCounter().GetValue()
			}
		}
		return 0
	}
	count := metricValue()
	delete(l.Tiers, "pro")
	if p, err := q.Check(ctx, "bl", "u", now); err != nil || p == nil || p.Tier != "" || p.Limit != usage.Yuan(1) {
		t.Fatalf("removed configuration: %+v, %v", p, err)
	}
	if metricValue() != count+1 {
		t.Fatal("unknown tier not counted")
	}
	// 仅档位配置开启配额；没有分配的 EndUser 仍不限。
	q.Limits["bl"] = usage.Limits{Tiers: map[string]usage.TierLimits{"pro": {EndUserDaily: 3}}}
	if periods, err := q.Status(ctx, "bl", "u", now); err != nil || len(periods) != 1 || periods[0].Tier != "pro" {
		t.Fatalf("tier-only quota: %+v, %v", periods, err)
	}
	if err := tiers.Delete(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	if periods, err := q.Status(ctx, "bl", "u", now); err != nil || len(periods) != 0 {
		t.Fatalf("tier-only default: %+v, %v", periods, err)
	}
}

type tierRead struct {
	tier string
	err  error
}

type delayedTierRead struct {
	usage.TierStore
	started chan struct{}
	release chan struct{}
	queries atomic.Int32
}

func (s *delayedTierRead) Get(ctx context.Context, bl, eu string) (string, error) {
	v, err := s.TierStore.Get(ctx, bl, eu)
	if s.queries.Add(1) == 1 {
		close(s.started)
		<-s.release
	}
	return v, err
}

func TestTierCacheCannotRefillAfterMutation(t *testing.T) {
	for _, action := range []string{"put", "delete", "uncertain_put", "uncertain_delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			clk := clock.NewFake(time.Unix(1000, 0))
			raw := usage.NewMemTiers()
			if err := raw.Put(ctx, "bl", "u", "old", clk.Now()); err != nil {
				t.Fatal(err)
			}
			delayed := &delayedTierRead{TierStore: raw, started: make(chan struct{}), release: make(chan struct{})}
			var inner usage.TierStore = delayed
			if strings.HasPrefix(action, "uncertain_") {
				inner = uncertainTiers{TierStore: delayed}
			}
			cache := usage.NewTierCache(inner, clk)
			result := make(chan tierRead, 1)
			go func() {
				v, err := cache.Get(ctx, "bl", "u")
				result <- tierRead{v, err}
			}()
			<-delayed.started
			want := "new"
			var err error
			if strings.HasSuffix(action, "delete") {
				want = ""
				err = cache.Delete(ctx, "bl", "u")
			} else {
				err = cache.Put(ctx, "bl", "u", want, clk.Now())
			}
			close(delayed.release)
			if (err != nil) != strings.HasPrefix(action, "uncertain_") {
				t.Fatalf("mutation error: %v", err)
			}
			got := <-result
			if got.err != nil || got.tier != want {
				t.Fatalf("in-flight read = %+v, want %q", got, want)
			}
			if v, err := cache.Get(ctx, "bl", "u"); err != nil || v != want || delayed.queries.Load() != 2 {
				t.Fatalf("cache refilled old tier: %q, %v, queries %d", v, err, delayed.queries.Load())
			}
		})
	}
}

type uncertainTiers struct{ usage.TierStore }

func (s uncertainTiers) Put(ctx context.Context, bl, eu, tier string, at time.Time) error {
	return errors.Join(s.TierStore.Put(ctx, bl, eu, tier, at), errors.New("acknowledgement lost"))
}

func (s uncertainTiers) Delete(ctx context.Context, bl, eu string) error {
	return errors.Join(s.TierStore.Delete(ctx, bl, eu), errors.New("acknowledgement lost"))
}

func TestTierCacheTTLAndLocalMutation(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	raw := usage.NewMemTiers()
	cache := usage.NewTierCache(raw, clk)
	get := func(want string) {
		t.Helper()
		if v, err := cache.Get(ctx, "bl", "u"); err != nil || v != want {
			t.Fatalf("Get = %q, %v, want %q", v, err, want)
		}
	}
	get("")
	if err := raw.Put(ctx, "bl", "u", "remote", clk.Now()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(usage.TierCacheTTL - time.Nanosecond)
	get("")
	clk.Advance(time.Nanosecond)
	get("remote")
	if err := cache.Put(ctx, "bl", "u", "local", clk.Now()); err != nil {
		t.Fatal(err)
	}
	get("local")
	if err := cache.Delete(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	get("")
	if err := raw.Put(ctx, "bl", "u", "other_process", clk.Now()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(usage.TierCacheTTL)
	get("other_process")
}

type failedTierRead struct {
	usage.TierStore
	err     error
	queries int
}

func (s *failedTierRead) Get(ctx context.Context, bl, eu string) (string, error) {
	s.queries++
	if s.err != nil {
		return "", s.err
	}
	return s.TierStore.Get(ctx, bl, eu)
}

func TestTierReadFailureDoesNotBecomeDefaultQuota(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	inner := &failedTierRead{TierStore: usage.NewMemTiers(), err: errors.New("read failed")}
	cache := usage.NewTierCache(inner, clk)
	q := &usage.Quotas{Store: usage.NewMem(), Tiers: cache, Limits: map[string]usage.Limits{"bl": {EndUserDaily: 1}}}
	if _, err := q.Status(ctx, "bl", "u", clk.Now()); !errors.Is(err, inner.err) {
		t.Fatalf("tier read failure was ignored: %v", err)
	}
	inner.err = nil
	if p, err := q.Status(ctx, "bl", "u", clk.Now()); err != nil || len(p) != 1 || inner.queries != 2 {
		t.Fatalf("failed read was cached: %+v, %v, queries %d", p, err, inner.queries)
	}
}
