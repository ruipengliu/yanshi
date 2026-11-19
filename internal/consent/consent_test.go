package consent_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/consent"
	"yanshi/internal/consent/consenttest"
)

func TestMem(t *testing.T) {
	consenttest.Run(t, func(*testing.T) consent.Store { return consent.NewMem() })
}

// 缓存之上也须满足同一套语义：本进程的写入立即可见。
func TestCacheConsistency(t *testing.T) {
	consenttest.Run(t, func(*testing.T) consent.Store {
		return consent.NewCache(consent.NewMem(), clock.NewFake(time.Unix(1000, 0)))
	})
}

func TestCacheTTLAndLocalWithdrawal(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	raw := consent.NewMem()
	cache := consent.NewCache(raw, clk)
	granted := func(want bool) {
		t.Helper()
		if at, err := cache.Get(ctx, "bl", "u", consent.Health); err != nil || at.IsZero() == want {
			t.Fatalf("Get = %v, %v; want granted %v", at, err, want)
		}
	}
	granted(false)
	// 其他进程的登记在缓存期内不可见，过期后可见。
	if err := raw.Put(ctx, "bl", "u", consent.Health, clk.Now()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(consent.CacheTTL - time.Nanosecond)
	granted(false)
	clk.Advance(time.Nanosecond)
	granted(true)
	// 本进程撤回立即生效。
	if err := cache.Delete(ctx, "bl", "u", consent.Health); err != nil {
		t.Fatal(err)
	}
	granted(false)
	if err := cache.Put(ctx, "bl", "u", consent.Health, clk.Now()); err != nil {
		t.Fatal(err)
	}
	granted(true)
	if err := cache.DeleteEndUser(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	granted(false)
	if err := cache.Put(ctx, "bl", "u", consent.Health, clk.Now()); err != nil {
		t.Fatal(err)
	}
	granted(true)
	// 其他进程撤回：至多 CacheTTL 后生效；Invalidate（Janitor）立即生效。
	if err := raw.Delete(ctx, "bl", "u", consent.Health); err != nil {
		t.Fatal(err)
	}
	granted(true)
	cache.Invalidate("bl", "u")
	granted(false)
}

type delayedRead struct {
	consent.Store
	started chan struct{}
	release chan struct{}
	queries atomic.Int32
}

func (s *delayedRead) Get(ctx context.Context, bl, eu string, kind consent.Kind) (time.Time, error) {
	v, err := s.Store.Get(ctx, bl, eu, kind)
	if s.queries.Add(1) == 1 {
		close(s.started)
		<-s.release
	}
	return v, err
}

// 撤回之前发起的读不能把旧的同意回填进缓存，也不能交给调用方。
func TestCacheCannotRefillAfterWithdrawal(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	raw := consent.NewMem()
	if err := raw.Put(ctx, "bl", "u", consent.Health, clk.Now()); err != nil {
		t.Fatal(err)
	}
	delayed := &delayedRead{Store: raw, started: make(chan struct{}), release: make(chan struct{})}
	cache := consent.NewCache(delayed, clk)
	result := make(chan time.Time, 1)
	go func() {
		at, _ := cache.Get(ctx, "bl", "u", consent.Health)
		result <- at
	}()
	<-delayed.started
	if err := cache.Delete(ctx, "bl", "u", consent.Health); err != nil {
		t.Fatal(err)
	}
	close(delayed.release)
	if at := <-result; !at.IsZero() {
		t.Fatalf("in-flight read returned withdrawn consent %v", at)
	}
	if at, err := cache.Get(ctx, "bl", "u", consent.Health); err != nil || !at.IsZero() || delayed.queries.Load() != 2 {
		t.Fatalf("cache refilled withdrawn consent: %v, %v, queries %d", at, err, delayed.queries.Load())
	}
}

type failedRead struct {
	consent.Store
	err error
}

func (s *failedRead) Get(ctx context.Context, bl, eu string, kind consent.Kind) (time.Time, error) {
	if s.err != nil {
		return time.Time{}, s.err
	}
	return s.Store.Get(ctx, bl, eu, kind)
}

func TestCacheDoesNotCacheFailures(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	raw := consent.NewMem()
	inner := &failedRead{Store: raw, err: errors.New("read failed")}
	cache := consent.NewCache(inner, clk)
	if _, err := cache.Get(ctx, "bl", "u", consent.Health); !errors.Is(err, inner.err) {
		t.Fatalf("read failure was hidden: %v", err)
	}
	if err := raw.Put(ctx, "bl", "u", consent.Health, clk.Now()); err != nil {
		t.Fatal(err)
	}
	inner.err = nil
	if at, err := cache.Get(ctx, "bl", "u", consent.Health); err != nil || at.IsZero() {
		t.Fatalf("failed read was cached: %v, %v", at, err)
	}
}
