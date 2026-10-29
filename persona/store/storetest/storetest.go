// Package storetest 是存储实现共用的一致性套件；只通过公开事务接口验证行为。
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

func Run(t *testing.T, factory func() store.Store) {
	t.Helper()
	t.Run("deletion_discovery", func(t *testing.T) { deletionDiscovery(t, factory) })
	t.Run("owner_deletion", func(t *testing.T) { ownerDeletion(t, factory) })
	t.Run("deletion_interleavings", func(t *testing.T) { deletionInterleavings(t, factory) })
	t.Run("deletion_read", func(t *testing.T) { deletionRead(t, factory) })
	t.Run("expiration", func(t *testing.T) { expiration(t, factory) })
	t.Run("captures", func(t *testing.T) {
		captures(t, factory)
	})
	t.Run("capture_numeric_order", func(t *testing.T) { captureNumericOrder(t, factory) })
	t.Run("deletions", func(t *testing.T) { deletions(t, factory) })
	t.Run("deletion_rebuild", func(t *testing.T) { deletionRebuild(t, factory) })
	t.Run("deletion_owners", func(t *testing.T) { deletionOwners(t, factory) })
	t.Run("reset_owner", func(t *testing.T) { resetOwner(t, factory) })
	ctx := context.Background()
	at := time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC)
	state := func(t *testing.T, s store.Store, bl, eu string) domain.State {
		t.Helper()
		var result domain.State
		if err := s.View(ctx, bl, eu, func(b *domain.Book) error { result = b.State(); return nil }); err != nil {
			t.Fatal(err)
		}
		return result
	}
	t.Run("isolation", func(t *testing.T) {
		s := factory()
		var escaped *domain.Book
		seed := domain.SeedData{Projects: []domain.Project{{ID: "P-1", Name: "项目", NextSteps: []string{"初值"}}}, Commitments: []domain.SeedCommitment{{ID: "C-1", Kind: "mine", Content: "发报价", Revisions: []domain.SeedRevision{{Fields: map[string]string{"due_date": "2026-10-28"}}}}}}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			b.Seed(seed, at)
			b.BindWeek("2026-10-17", "week")
			escaped = b
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		seed.Projects[0].NextSteps[0] = "改外部输入"
		escaped.Bind("P-1", "escaped")
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			st := b.State()
			st.Projects[0].NextSteps[0] = "改读取快照"
			st.Changesets[1].ReviseCommitments[0].Fields["due_date"] = "changed"
			b.BindWeek("2026-10-17", "changed")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		st := state(t, s, "bl", "u")
		if st.Projects[0].NextSteps[0] != "初值" || st.Projects[0].Session != "" || st.Commitments[0].DueDate != "2026-10-28" {
			t.Fatalf("leaked aliases: %+v", st)
		}
		for _, key := range [][2]string{{"bl", "v"}, {"other", "u"}, {"bl\x00u", ""}, {"bl", "u\x00"}} {
			if st := state(t, s, key[0], key[1]); len(st.Projects) != 0 || len(st.Commitments) != 0 {
				t.Fatal("cross-key data leak")
			}
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			if b.ReviewWeek(at).Session != "week" {
				t.Fatal("week binding leaked")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		s := factory()
		fail := errors.New("rollback")
		err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			b.Seed(domain.SeedData{Commitments: []domain.SeedCommitment{{Kind: "mine", Content: "发报价"}}}, at)
			b.BindWeek("2026-10-17", "week")
			b.Submitted("day", "整理交谈")
			return fail
		})
		if !errors.Is(err, fail) {
			t.Fatalf("error = %v", err)
		}
		if st := state(t, s, "bl", "u"); len(st.Commitments) != 0 {
			t.Fatal("partial write survived")
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if b.ReviewWeek(at).Session != "" {
				t.Fatal("binding survived rollback")
			}
			c := domain.NewCall(b, "day", []byte(`{"day":{"headline":"今日"}}`), nil, at)
			c.FileDay()
			if c.Filed == nil {
				t.Fatal("submitted marker survived rollback")
			}
			b.Seed(domain.SeedData{Commitments: []domain.SeedCommitment{{Kind: "mine", Content: "带样板"}}}, at)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if st := state(t, s, "bl", "u"); st.Commitments[0].ID != "C-0001" || st.Changesets[0].ID != "CS-0001" {
			t.Fatalf("allocation survived rollback: %+v", st)
		}
	})
	t.Run("concurrent_updates", func(t *testing.T) {
		s := factory()
		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.Update(ctx, "bl", "u", func(b *domain.Book) error {
					b.Seed(domain.SeedData{Commitments: []domain.SeedCommitment{{Kind: "mine", Content: fmt.Sprintf("item-%d", i)}}}, at)
					return nil
				})
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		st := state(t, s, "bl", "u")
		if len(st.Commitments) != 40 || len(st.Changesets) != 40 {
			t.Fatalf("lost updates: %+v", st)
		}
		seen := map[string]bool{}
		for _, c := range st.Commitments {
			if seen[c.ID] {
				t.Fatal("duplicate id")
			}
			seen[c.ID] = true
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		s := factory()
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		for _, fn := range []func(context.Context, string, string, func(*domain.Book) error) error{s.Update, s.View} {
			if err := fn(cancelled, "bl", "u", func(*domain.Book) error { t.Fatal("cancelled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
		}
	})
}
