package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

func expiration(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	s := factory()
	seedCaptures(t, s)
	if err := s.Update(ctx, "empty", "u", func(b *domain.Book) error { return b.BindDay(captureDate, "empty") }); err != nil {
		t.Fatal(err)
	}
	expires, _ := domain.CaptureExpiresAt(captureDate, "UTC")
	if owners, err := s.ExpiringCaptures(ctx, expires.Add(-time.Nanosecond)); err != nil || len(owners) != 0 {
		t.Fatal(owners, err)
	}
	want := []store.Owner{{BusinessLine: "bl", EndUser: "u"}}
	if owners, err := s.ExpiringCaptures(ctx, expires); err != nil || !reflect.DeepEqual(owners, want) {
		t.Fatal(owners, err)
	}
	failure := errors.New("rollback")
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if err := b.ExpireCaptures(captureDate, expires); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		b.SetCaptureClock(func() time.Time { return expires.Add(-time.Nanosecond) })
		segments, err := b.DayCaptures(captureDate)
		if err != nil || len(segments) != 3 || b.BusinessDay(captureDate).Expired {
			t.Fatal("rollback leaked cleanup", err)
		}
		b.SetCaptureClock(func() time.Time { return expires })
		if _, err := b.DayCaptures(captureDate); !errors.Is(err, domain.ErrCapturesExpired) {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error { return b.ExpireCaptures(captureDate, expires) }); err != nil {
			t.Fatal(err)
		}
	}
	if owners, err := s.ExpiringCaptures(ctx, expires); err != nil || len(owners) != 0 {
		t.Fatal(owners, err)
	}
	var escaped domain.CaptureAccess
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		day := b.BusinessDay(captureDate)
		if !day.Expired || day.Deleted || day.CaptureRevision != 2 || day.Session != "day" || day.TimeZone != "UTC" {
			t.Fatal(day)
		}
		// 即使测试时钟拨回期限之前，已清理状态也不能重新开放。
		b.SetCaptureClock(func() time.Time { return captureNow })
		if _, err := b.DayCaptures(captureDate); !errors.Is(err, domain.ErrCapturesExpired) {
			t.Fatal(err)
		}
		escaped = b.CaptureAccess()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := escaped.Lookup(captureDate, domain.Ref{Seq: 1}); !errors.Is(err, domain.ErrCaptureTransactionClosed) {
		t.Fatal("lifetime must win over expiry", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ExpiringCaptures(cancelled, expires); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
