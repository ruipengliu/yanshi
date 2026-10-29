package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
	"persona/store/memstore"
)

type expirationStore struct {
	store.Store
	mu        sync.Mutex
	fail      bool
	scanned   chan struct{}
	committed chan struct{}
}

func (s *expirationStore) ExpiringCaptures(ctx context.Context, now time.Time) ([]store.Owner, error) {
	owners, err := s.Store.ExpiringCaptures(ctx, now)
	s.scanned <- struct{}{}
	return owners, err
}
func (s *expirationStore) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	err := s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		if err := fn(b); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.fail {
			s.fail = false
			return errors.New("cleanup unavailable")
		}
		return nil
	})
	if err == nil {
		s.committed <- struct{}{}
	}
	return err
}
func TestExpirationLoopRecovery(t *testing.T) {
	clock := newDeletionClock()
	books := memstore.New(clock.now)
	ctx := context.Background()
	const date = "2026-10-27"
	if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
		_, err := b.AppendCaptures(date, "a", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文", TimeZone: "UTC"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expires, _ := domain.CaptureExpiresAt(date, "UTC")
	clock.mu.Lock()
	clock.at = expires
	clock.mu.Unlock()
	s := &expirationStore{Store: books, fail: true, scanned: make(chan struct{}, 8), committed: make(chan struct{}, 8)}
	first := New(ctx, s, nil, Options{Now: clock.now})
	if err := first.ScanExpirations(ctx); err == nil {
		t.Fatal("failure hidden")
	}
	finish(t, first)
	<-s.scanned
	// 重启无需通知就恢复到期的 owner；后续由可注入 tick 驱动。
	second := New(ctx, s, nil, Options{Now: clock.now})
	defer finish(t, second)
	ticks := make(chan time.Time, 1)
	second.StartExpirations(ticks)
	select {
	case <-s.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not clean")
	}
	<-s.scanned
	ticks <- expires
	select {
	case <-s.scanned:
	case <-time.After(5 * time.Second):
		t.Fatal("periodic scan missing")
	}
	finish(t, second)
	if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
		day := b.BusinessDay(date)
		if !day.Expired || day.CaptureRevision != 2 {
			t.Fatal(day)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := second.ScanExpirations(ctx); err == nil {
		t.Fatal("accepted after shutdown")
	}
}

func TestEnsureDaySessionUsesExpiryBeforeReuse(t *testing.T) {
	clock := newDeletionClock()
	books := memstore.New(clock.now)
	ctx := context.Background()
	if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
		_, err := b.AppendCaptures("2026-10-28", "a", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文", TimeZone: "America/New_York"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	k := &deletionKernel{}
	o := New(ctx, books, k, Options{Now: clock.now})
	defer finish(t, o)
	sid, err := o.EnsureDaySession(ctx, "bl", "u", "2026-10-28")
	if err != nil || sid == "" {
		t.Fatal(sid, err)
	}
	expires, _ := domain.CaptureExpiresAt("2026-10-28", "America/New_York")
	if !k.deadline.Equal(expires) {
		t.Fatal(k.deadline, expires)
	}
	clock.mu.Lock()
	clock.at = expires
	clock.mu.Unlock()
	if _, err := o.EnsureDaySession(ctx, "bl", "u", "2026-10-28"); !errors.Is(err, domain.ErrCapturesExpired) {
		t.Fatal("expired binding reused", err)
	}
}
