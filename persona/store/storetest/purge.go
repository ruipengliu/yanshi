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

func assertOwnerBlocked(t *testing.T, s store.Store) {
	t.Helper()
	for _, transact := range []func(context.Context, string, string, func(*domain.Book) error) error{s.Update, s.View} {
		if err := transact(context.Background(), "bl", "u", func(*domain.Book) error {
			t.Error("barrier allowed business callback")
			return nil
		}); !errors.Is(err, store.ErrOwnerDeleting) {
			t.Fatal("missing owner barrier", err)
		}
	}
	if r, ok := s.(store.Resetter); ok {
		if err := r.ResetOwner(context.Background(), "bl", "u"); !errors.Is(err, store.ErrOwnerDeleting) {
			t.Fatal("fixture reset bypassed barrier", err)
		}
	}
}

func finishPurge(t *testing.T, s store.Store) store.OwnerDeletion {
	t.Helper()
	for range 100 {
		out, err := s.Purge(context.Background(), "bl", "u", 1, captureNow.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		assertOwnerBlocked(t, s)
		if out.Status == "completed" {
			return out
		}
	}
	t.Fatal("purge did not converge")
	return store.OwnerDeletion{}
}

func ownerDeletion(t *testing.T, factory func() store.Store) {
	s := factory()
	ctx := context.Background()
	seedCaptures(t, s)
	if _, err := s.Purge(ctx, "bl", "u", 1, captureNow); !errors.Is(err, store.ErrDeletionNotStarted) {
		t.Fatal("purge without barrier", err)
	}
	if _, err := s.OwnerDeletion(ctx, "bl", "u"); !errors.Is(err, store.ErrDeletionNotStarted) {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1", Name: "项目"}}, Commitments: []domain.SeedCommitment{{Kind: "mine", Content: "事项"}}}, captureNow)
		b.BindWeek("2026-10-21", "week")
		b.Submitted("day", "整理今天")
		_, err := domain.NewCall(b, "day", nil, nil, captureNow).Captures(captureOptions())
		if err != nil {
			return err
		}
		_, err = b.Erase(captureDate, "1.0", "1.0", captureNow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.BeginOwnerDeletion(cancelled, "bl", "u", captureNow); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.OwnerDeletion(ctx, "bl", "u"); !errors.Is(err, store.ErrDeletionNotStarted) {
		t.Fatal("cancelled barrier persisted", err)
	}
	started, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow)
	if err != nil || started.Status != "pending" || !started.StartedAt.Equal(captureNow) || !started.CompletedAt.IsZero() {
		t.Fatal(started, err)
	}
	assertOwnerBlocked(t, s)
	repeat, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow.Add(time.Hour))
	if err != nil || !repeat.StartedAt.Equal(started.StartedAt) {
		t.Fatal("repeated barrier changed start", err)
	}
	want := []store.Owner{{BusinessLine: "bl", EndUser: "u"}}
	if owners, err := s.PendingPurges(ctx); err != nil || !reflect.DeepEqual(owners, want) {
		t.Fatal(owners, err)
	}
	if owners, err := s.PendingDeletions(ctx); err != nil || len(owners) != 0 {
		t.Fatal("deletion scanner rediscovered blocked owner", owners, err)
	}
	if owners, err := s.ExpiringCaptures(ctx, captureNow.AddDate(0, 0, 20)); err != nil || len(owners) != 0 {
		t.Fatal("expiry scanner rediscovered blocked owner", owners, err)
	}
	for _, limit := range []int{0, -1, store.MaxPurgeBatch + 1} {
		if _, err := s.Purge(ctx, "bl", "u", limit, captureNow); !errors.Is(err, store.ErrPurgeLimit) {
			t.Fatal(err)
		}
	}
	if _, err := s.Purge(cancelled, "bl", "u", 1, captureNow); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	first, err := s.Purge(ctx, "bl", "u", 1, captureNow)
	if err != nil || first.Status != "pending" {
		t.Fatal("one row batch cleared whole owner", first, err)
	}
	// 驱动不持有恢复状态；中断后用同一存储的新实例逐批推进。
	for range 100 {
		if err := (store.Purger{Store: s, Now: func() time.Time { return captureNow.Add(time.Hour) }, BatchSize: 1}).Scan(ctx); err != nil {
			t.Fatal(err)
		}
		progress, err := s.OwnerDeletion(ctx, "bl", "u")
		if err != nil {
			t.Fatal(err)
		}
		if progress.Status == "completed" {
			break
		}
	}
	completed, err := s.OwnerDeletion(ctx, "bl", "u")
	if err != nil || completed.Status != "completed" || !completed.CompletedAt.Equal(captureNow.Add(time.Hour)) {
		t.Fatal(completed, err)
	}
	for range 2 {
		got, err := s.Purge(ctx, "bl", "u", 1, captureNow.Add(2*time.Hour))
		if err != nil || !got.CompletedAt.Equal(completed.CompletedAt) {
			t.Fatal("completed purge changed", got, err)
		}
		got, err = s.BeginOwnerDeletion(ctx, "bl", "u", captureNow.Add(2*time.Hour))
		if err != nil || got.Status != "completed" || !got.StartedAt.Equal(started.StartedAt) {
			t.Fatal("barrier restarted", got, err)
		}
	}
	assertOwnerBlocked(t, s)
	if owners, err := s.PendingPurges(ctx); err != nil || len(owners) != 0 {
		t.Fatal(owners, err)
	}
	for _, owner := range []store.Owner{{BusinessLine: "other", EndUser: "u"}, {BusinessLine: "bl", EndUser: "new-id"}} {
		if err := s.Update(ctx, owner.BusinessLine, owner.EndUser, func(b *domain.Book) error {
			_, err := b.AppendCaptures(captureDate, "new", []domain.CaptureSegment{{Speaker: "wearer", Text: "新账号"}})
			return err
		}); err != nil {
			t.Fatal("barrier crossed owner boundary", err)
		}
	}
}

func deletionInterleavings(t *testing.T, factory func() store.Store) {
	operations := map[string]func(*domain.Book) error{
		"append": func(b *domain.Book) error {
			_, err := b.AppendCaptures(captureDate, "late", []domain.CaptureSegment{{Speaker: "wearer", Text: "在途追加"}})
			return err
		},
		"file_day": func(b *domain.Book) error {
			call := domain.NewCall(b, "day", []byte(`{"day":{"headline":"整理"}}`), nil, captureNow)
			call.FileDay()
			return call.Err()
		},
		"erase": func(b *domain.Book) error {
			_, err := b.Erase(captureDate, "1.0", "1.0", captureNow)
			return err
		},
		"expiry": func(b *domain.Book) error { return b.ExpireCaptures(captureDate, captureNow.AddDate(0, 0, 20)) },
	}
	operations["first_append"] = operations["append"]
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			s := factory()
			if name != "first_append" {
				seedCaptures(t, s)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			result := make(chan error, 1)
			go func() {
				result <- s.Update(ctx, "bl", "u", func(b *domain.Book) error {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					return operation(b)
				})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			barrier := make(chan error, 1)
			go func() {
				_, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow)
				barrier <- err
			}()
			select {
			case err := <-barrier:
				t.Fatal("barrier passed uncommitted write", err)
			case <-time.After(30 * time.Millisecond):
			}
			release <- struct{}{}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if err := <-barrier; err != nil {
				t.Fatal(err)
			}
			if err := s.Update(ctx, "bl", "u", operation); !errors.Is(err, store.ErrOwnerDeleting) {
				t.Fatal("late operation bypassed barrier", err)
			}
			finishPurge(t, s)
		})
	}
}

func deletionRead(t *testing.T, factory func() store.Store) {
	s := factory()
	seedCaptures(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	result := make(chan error, 1)
	go func() {
		result <- s.View(ctx, "bl", "u", func(b *domain.Book) error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			// 注销不篡改在途快照，但最终 View 必须拒绝交付这个结果。
			segments, err := b.DayCaptures(captureDate)
			if err != nil || len(segments) != 3 {
				t.Error("purge mutated read snapshot")
			}
			// 到期错误允许开发入口展示元数据；注销必须覆盖它，不能泄露旧 Day。
			return domain.ErrCapturesExpired
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow); err != nil {
		t.Fatal(err)
	}
	finishPurge(t, s)
	release <- struct{}{}
	if err := <-result; !errors.Is(err, store.ErrOwnerDeleting) {
		t.Fatal("in-flight read escaped barrier", err)
	}
}

func deletionDiscovery(t *testing.T, factory func() store.Store) {
	s := factory()
	ctx := context.Background()
	keys := []store.Owner{{BusinessLine: "bl", EndUser: "u\x00"}, {BusinessLine: "a", EndUser: "z"}, {BusinessLine: "bl", EndUser: "u"}}
	for _, key := range keys {
		if _, err := s.BeginOwnerDeletion(ctx, key.BusinessLine, key.EndUser, captureNow); err != nil {
			t.Fatal(err)
		}
	}
	want := []store.Owner{keys[1], keys[2], keys[0]}
	if got, err := s.PendingPurges(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("owner discovery order", got, err)
	}
	if _, err := s.Purge(ctx, "bl", "u", 1, captureNow); err != nil {
		t.Fatal(err)
	}
	want = []store.Owner{keys[1], keys[0]}
	if got, err := s.PendingPurges(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("discovery retained completed owner", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PendingPurges(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.OwnerDeletion(cancelled, "bl", "u"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
