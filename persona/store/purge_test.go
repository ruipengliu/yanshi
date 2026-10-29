package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"persona/store"
	"persona/store/memstore"
)

type failedPurge struct {
	store.Store
	fail    string
	failure error
}

func (s failedPurge) Purge(ctx context.Context, bl, eu string, limit int, now time.Time) (store.OwnerDeletion, error) {
	if eu == s.fail {
		return store.OwnerDeletion{}, s.failure
	}
	return s.Store.Purge(ctx, bl, eu, limit, now)
}

func TestPurgerRetriesAndContinues(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s := memstore.New()
	for _, eu := range []string{"a", "b"} {
		if _, err := s.BeginOwnerDeletion(ctx, "bl", eu, now); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("injected failure")
	p := store.Purger{Store: failedPurge{Store: s, fail: "a", failure: failure}, Now: func() time.Time { return now.Add(time.Hour) }, BatchSize: 1}
	if err := p.Scan(ctx); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if out, err := s.OwnerDeletion(ctx, "bl", "b"); err != nil || out.Status != "completed" || !out.CompletedAt.Equal(now.Add(time.Hour)) {
		t.Fatal("one owner blocked another", out, err)
	}
	if owners, err := s.PendingPurges(ctx); err != nil || len(owners) != 1 || owners[0].EndUser != "a" {
		t.Fatal(owners, err)
	}
	// 新驱动没有上轮内存，仍能发现失败项，注入的时钟决定完成时间。
	if err := (store.Purger{Store: s, Now: func() time.Time { return now.Add(2 * time.Hour) }}).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if out, err := s.OwnerDeletion(ctx, "bl", "a"); err != nil || !out.CompletedAt.Equal(now.Add(2*time.Hour)) {
		t.Fatal(out, err)
	}
}
