package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

func workSessions(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, target := range []string{"day/2026-10-28", "project/p", "week/2026-10-22"} {
		t.Run(target, func(t *testing.T) {
			s := factory()
			update := func(fn func(*domain.Book) error) error {
				return s.Update(ctx, "bl", "u", func(b *domain.Book) error {
					b.SetCaptureClock(func() time.Time {
						return now
					})
					return fn(b)
				})
			}
			if err := update(func(b *domain.Book) error {
				b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}, {ID: "q"}}}, now)
				_, err := b.AppendCaptures("2026-10-28", "c", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文", TimeZone: "UTC"}})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			lease := domain.WorkLease{Task: "task", Sequence: 1, Generation: 1, Until: now.Add(time.Minute)}
			var expected domain.WorkTarget
			if err := update(func(b *domain.Book) error {
				var err error
				expected, err = b.ReserveWork(target, lease, now)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// 同一读取快照的两个创建者，只有一个能把 Session 变成当前绑定。
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, sid := range []string{"a", "b"} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results <- update(func(b *domain.Book) error {
						return b.BindWork(target, expected, lease, sid, now)
					})
				}()
			}
			wg.Wait()
			close(results)
			wins := 0
			for err := range results {
				if err == nil {
					wins++
				} else if !errors.Is(err, domain.ErrWorkConflict) {
					t.Fatal(err)
				}
			}
			if wins != 1 {
				t.Fatal("CAS winners", wins)
			}
			var old string
			kind := "day"
			if strings.HasPrefix(target, "project/") {
				kind = "collect"
			}
			if strings.HasPrefix(target, "week/") {
				kind = "week"
			}
			if err := update(func(b *domain.Book) error {
				current, err := b.WorkTarget(target)
				if err != nil {
					return err
				}
				old = current.Session
				if err := b.RegisterWorkCommand(target, old, "one", kind, lease, now); err != nil {
					return err
				}
				if err := b.RegisterWorkCommand(target, old, "two", kind, lease, now); !errors.Is(err, domain.ErrWorkBusy) {
					t.Fatal("changed active command", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := update(func(b *domain.Book) error {
				return b.RetireWork(target, old, now)
			}); err != nil {
				t.Fatal(err)
			}
			lease.Generation++
			lease.Until = now.Add(2 * time.Minute)
			if err := update(func(b *domain.Book) error {
				current, err := b.ReserveWork(target, lease, now)
				if err != nil {
					return err
				}
				if !current.DeleteAt.Equal(expected.DeleteAt) {
					t.Fatal("retirement extended deadline")
				}
				if err := b.BindWork(target, current, lease, "replacement", now); err != nil {
					return err
				}
				return b.RegisterWorkCommand(target, "replacement", "two", kind, lease, now)
			}); err != nil {
				t.Fatal(err)
			}
			// 事务重新加载历史；旧请求在下一命令之后才到达，也不能写入或读取原文。
			if err := update(func(b *domain.Book) error {
				if err := b.CheckWorkSession(old); !errors.Is(err, domain.ErrWorkRetired) {
					t.Fatal(err)
				}
				call := domain.NewCall(b, old, []byte(`{}`), nil, now)
				for _, write := range []func() string{call.FileChanges, call.FileDay, call.FileWeek, call.ProposeMerge, call.SetStatus, call.UpdateCommitments} {
					if got := write(); got != domain.ErrWorkRetired.Error() {
						t.Fatal("late write", got)
					}
				}
				if _, err := call.Captures(domain.PageOptions{}); !errors.Is(err, domain.ErrWorkRetired) {
					t.Fatal("late raw read", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("expired_lease_and_orphan_fence", func(t *testing.T) {
		s := factory()
		lease := domain.WorkLease{Task: "task", Sequence: 1, Generation: 1, Until: now.Add(time.Minute)}
		var expected domain.WorkTarget
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}}}, now)
			var err error
			expected, err = b.ReserveWork("project/p", lease, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			return b.BindWork("project/p", expected, lease, "late", lease.Until)
		}); !errors.Is(err, domain.ErrWorkConflict) {
			t.Fatal("expired lease", err)
		}
		newLease := lease
		newLease.Generation++
		newLease.Until = now.Add(3 * time.Minute)
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if _, err := b.ReserveWork("project/p", newLease, now); err != nil {
				return err
			}
			if !b.FenceOrphan("orphan") {
				t.Fatal("fence failed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			lease domain.WorkLease
			sid   string
		}{{lease, "late"}, {newLease, "orphan"}} {
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				return b.BindWork("project/p", expected, tc.lease, tc.sid, now)
			}); !errors.Is(err, domain.ErrWorkConflict) {
				t.Fatal("late bind", err)
			}
		}
	})
}

func workFactRecovery(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s := factory()
	lease := domain.WorkLease{Task: "task", Sequence: 1, Generation: 1, Until: now.Add(time.Minute)}
	file := func(session, command string) error {
		return s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			expected, err := b.ReserveWork("week/2026-10-22", lease, now)
			if err != nil {
				return err
			}
			if expected.Session == "" {
				if err := b.BindWork("week/2026-10-22", expected, lease, session, now); err != nil {
					return err
				}
			}
			if err := b.RegisterWorkCommand("week/2026-10-22", session, command, "week", lease, now); err != nil {
				return err
			}
			call := domain.NewCall(b, session, []byte(`{}`), nil, now)
			call.FileWeek()
			return call.Err()
		})
	}
	if err := file("old", "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		return b.RetireWork("week/2026-10-22", "old", now)
	}); err != nil {
		t.Fatal(err)
	}
	lease.Generation++
	if err := file("new", "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if len(b.PendingDomainChanges()) != 1 {
			t.Fatal("same command replay emitted another fact")
		}
		return b.FinishWorkCommand("new", "one")
	}); err != nil {
		t.Fatal(err)
	}
	if err := file("new", "two"); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		if len(b.PendingDomainChanges()) != 2 {
			t.Fatal("new command swallowed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func workProjectRecovery(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s := factory()
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}}}, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, sid := range []string{"old", "new"} {
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			lease := domain.WorkLease{Task: "task", Sequence: 1, Generation: int64(i + 1), Until: now.Add(time.Minute)}
			if i > 0 {
				if err := b.RetireWork("project/p", "old", now); err != nil {
					return err
				}
			}
			expected, err := b.ReserveWork("project/p", lease, now)
			if err != nil {
				return err
			}
			if err := b.BindWork("project/p", expected, lease, sid, now); err != nil {
				return err
			}
			if err := b.RegisterWorkCommand("project/p", sid, "one", "collect", lease, now); err != nil {
				return err
			}
			call := domain.NewCall(b, sid, []byte(`{"date":"2026-10-28"}`), nil, now)
			call.UpdateCommitments()
			return call.Err()
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		if len(b.State().Changesets) != 1 {
			t.Fatal("project replay appended changeset", len(b.State().Changesets))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
