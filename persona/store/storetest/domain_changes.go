package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

func domainChanges(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	for _, kind := range []string{domain.FactDay, domain.FactWeek} {
		t.Run(kind, func(t *testing.T) {
			s := factory()
			date, sid := captureDate, "day"
			if kind == domain.FactWeek {
				date, sid = "2026-10-21", "week"
			}
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				if kind == domain.FactDay {
					return b.BindDay(date, sid)
				}
				b.BindWeek(date, sid)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			args := `{"day":{"headline":"今日事务"},"habits":[],"suggestions":[]}`
			apply := func(b *domain.Book, raw string, now time.Time) error {
				call := domain.NewCall(b, sid, []byte(raw), nil, now)
				if kind == domain.FactDay {
					call.FileDay()
					if call.Filed == nil {
						t.Error("day not accepted")
					}
				} else {
					call.FileWeek()
				}
				return call.Err()
			}
			failure := errors.New("callback failed")
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				if err := apply(b, args, captureNow); err != nil {
					return err
				}
				return failure
			}); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if changes, err := s.PendingDomainChanges(ctx, "bl", "u"); err != nil || len(changes) != 0 {
				t.Fatal("change survived rollback", changes, err)
			}
			if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
				if _, ok := b.AcceptedFact(kind, date); ok || len(b.State().Days)+len(b.State().Weeks) != 0 {
					t.Fatal("fact survived rollback")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			file := func(raw string, now time.Time) {
				t.Helper()
				if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error { return apply(b, raw, now) }); err != nil {
					t.Fatal(err)
				}
			}
			pending := func() []store.DomainChange {
				t.Helper()
				changes, err := s.PendingDomainChanges(ctx, "bl", "u")
				if err != nil {
					t.Fatal(err)
				}
				return changes
			}
			file(args, captureNow)
			first := pending()
			if len(first) != 1 || first[0].ID == "" || first[0].Version != 1 || first[0].Kind != kind || first[0].Date != date || first[0].Owner != (store.Owner{BusinessLine: "bl", EndUser: "u"}) {
				t.Fatal("missing accepted fact", first)
			}
			if !reflect.DeepEqual(first, pending()) {
				t.Fatal("read acknowledged change")
			}
			// 跨午夜且键顺序不同的重试仍是同一命令；FiledAt 不作为版本，也不因重试刷新。
			file(`{"suggestions":[],"habits":[],"day":{"headline":"今日事务"}}`, captureNow.Add(24*time.Hour))
			if !reflect.DeepEqual(first, pending()) {
				t.Fatal("retry changed acceptance", pending())
			}
			if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
				fact, ok := b.AcceptedFact(kind, date)
				if !ok || fact != first[0].Fact {
					t.Fatal("fact and change disagree", fact)
				}
				st := b.State()
				if kind == domain.FactDay && !st.Days[0].FiledAt.Equal(captureNow) || kind == domain.FactWeek && (!st.Weeks[0].FiledAt.Equal(captureNow) || len(st.Weeks[0].Habits) != 0) {
					t.Fatal("retry changed content timestamp or empty week was rejected")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// 同一时钟下的合法重交仍递增；过滤完的周回顾同样接受。
			changed := `{"day":{"headline":"修订事务"},"habits":[{"habit":"只出现一次","days":["2026-10-21"]}],"suggestions":[]}`
			file(changed, captureNow)
			if changes := pending(); len(changes) != 2 || changes[1].ID != first[0].ID || changes[1].Version != 2 {
				t.Fatal("resubmission lost version", changes)
			}
			file(args, captureNow)
			if changes := pending(); len(changes) != 3 || changes[2].Version != 3 {
				t.Fatal("return to earlier content was mistaken for retry", changes)
			}
			id := first[0].ID
			if err := s.AckDomainChanges(ctx, "bl", "u", id, 4, captureNow); err == nil {
				t.Fatal("future acknowledgement accepted")
			}
			for range 2 {
				if err := s.AckDomainChanges(ctx, "bl", "u", id, 2, captureNow); err != nil {
					t.Fatal(err)
				}
			}
			if changes := pending(); len(changes) != 1 || changes[0].Version != 3 {
				t.Fatal("acknowledgement swallowed a newer version", changes)
			}
			if err := s.AckDomainChanges(ctx, "bl", "u", id, 3, captureNow); err != nil {
				t.Fatal(err)
			}
			if owners, err := s.PendingChangeOwners(ctx); err != nil || len(owners) != 0 {
				t.Fatal("confirmed owner still discovered", owners, err)
			}
			// 清理确认历史后，重试仍去重，合法重交继续版本计数。
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				b.PruneDomainChanges(captureNow.Add(domain.DomainChangeRetention))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.AckDomainChanges(ctx, "bl", "u", id, 2, captureNow); err != nil {
				t.Fatal("ack replay after pruning failed", err)
			}
			file(args, captureNow)
			if len(pending()) != 0 {
				t.Fatal("pruning discarded deduplication")
			}
			file(changed, captureNow)
			if changes := pending(); len(changes) != 1 || changes[0].Version != 4 {
				t.Fatal("pruning reset version", changes)
			}
		})
	}
	t.Run("erasure_expiration", func(t *testing.T) {
		s := factory()
		seedCaptures(t, s)
		file := func(sid string) {
			t.Helper()
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				c := domain.NewCall(b, sid, []byte(`{"day":{"headline":"待擦除内容"}}`), nil, captureNow)
				c.FileDay()
				return c.Err()
			}); err != nil {
				t.Fatal(err)
			}
		}
		file("day")
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if _, err := b.Erase(captureDate, "1.0", "1.0", captureNow); err != nil {
				return err
			}
			if _, active := b.AcceptedFact(domain.FactDay, captureDate); active {
				t.Fatal("erased fact still active")
			}
			return b.BindDay(captureDate, "rebuilt")
		}); err != nil {
			t.Fatal(err)
		}
		file("rebuilt")
		changes, err := s.PendingDomainChanges(ctx, "bl", "u")
		if err != nil || len(changes) != 2 || changes[0].ID != changes[1].ID || changes[1].Version != 2 {
			t.Fatal("rebuild reused fact version", changes, err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if err := b.ExpireCaptures(captureDate, captureNow.AddDate(0, 0, 20)); err != nil {
				return err
			}
			if _, active := b.AcceptedFact(domain.FactDay, captureDate); !active || len(b.State().Days) != 1 {
				t.Fatal("expiration removed long-lived fact")
			}
			_, err := b.EraseDay(captureDate, captureNow.AddDate(0, 0, 20))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			if _, active := b.AcceptedFact(domain.FactDay, captureDate); active || len(b.State().Days) != 0 {
				t.Fatal("whole-day deletion retained fact")
			}
			data, err := json.Marshal(b)
			if err != nil || strings.Contains(string(data), "待擦除内容") {
				t.Fatal("erasure retained content", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		got, err := s.PendingDomainChanges(ctx, "bl", "u")
		if err != nil || !reflect.DeepEqual(got, changes) {
			t.Fatal("deletion/expiry lost unacknowledged identifiers", got, err)
		}
	})
	t.Run("discovery_barrier", func(t *testing.T) {
		s := factory()
		owners := []store.Owner{{BusinessLine: "bl", EndUser: "u\x00"}, {BusinessLine: "a", EndUser: "z"}, {BusinessLine: "bl", EndUser: "u"}}
		for _, owner := range owners {
			if err := s.Update(ctx, owner.BusinessLine, owner.EndUser, func(b *domain.Book) error {
				c := domain.NewCall(b, "week", []byte(`{}`), nil, captureNow)
				c.FileWeek()
				return c.Err()
			}); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := s.PendingChangeOwners(ctx); err != nil || !reflect.DeepEqual(got, []store.Owner{owners[1], owners[2], owners[0]}) {
			t.Fatal("discovery order", got, err)
		}
		if got, err := s.PendingDomainChanges(ctx, "other", "u"); err != nil || len(got) != 0 {
			t.Fatal("owner leak", got, err)
		}
		if _, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow); err != nil {
			t.Fatal(err)
		}
		if got, err := s.PendingChangeOwners(ctx); err != nil || !reflect.DeepEqual(got, []store.Owner{owners[1], owners[0]}) {
			t.Fatal("barrier did not hide changes", got, err)
		}
		finishPurge(t, s)
		if got, err := s.PendingDomainChanges(ctx, "bl", "u"); !errors.Is(err, store.ErrOwnerDeleting) || got != nil {
			t.Fatal("purged owner readable", got, err)
		}
		if err := s.AckDomainChanges(ctx, "bl", "u", "week/2026-10-22", 1, captureNow); !errors.Is(err, store.ErrOwnerDeleting) {
			t.Fatal("ack bypassed barrier", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.PendingChangeOwners(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if _, err := s.PendingDomainChanges(cancelled, "a", "z"); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := s.AckDomainChanges(cancelled, "a", "z", "week/2026-10-22", 1, captureNow); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("concurrent_retry", func(t *testing.T) {
		s := factory()
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.Update(ctx, "bl", "u", func(b *domain.Book) error {
					c := domain.NewCall(b, "week", []byte(`{}`), nil, captureNow)
					c.FileWeek()
					return c.Err()
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
		if changes, err := s.PendingDomainChanges(ctx, "bl", "u"); err != nil || len(changes) != 1 || changes[0].Version != 1 {
			t.Fatal("concurrent duplicate", changes, err)
		}
	})
}
