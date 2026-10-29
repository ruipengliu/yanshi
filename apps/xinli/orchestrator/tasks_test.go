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
	"xinli/pg"
	"xinli/pg/pgtest"
	"xinli/task"
	taskmem "xinli/task/memstore"
	taskpg "xinli/task/pgstore"
)

func collectionFixture(b *domain.Book, sid, project string, now time.Time) error {
	date := now.Format(time.DateOnly)
	b.Seed(domain.SeedData{Projects: []domain.Project{{ID: project}}, Exchanges: []domain.SeedExchange{{Date: date, Project: project}}}, now)
	if err := b.BindDay(date, "seed_"+date); err != nil {
		return err
	}
	if err := b.BindDay(date, sid); err != nil {
		return err
	}
	call := domain.NewCall(b, sid, []byte(`{"day":{"headline":"今日事务"}}`), nil, now)
	call.FileDay()
	return call.Err()
}

type ackLoss struct {
	store.Store
	mu    sync.Mutex
	fail  bool
	calls int
}

func (s *ackLoss) AckDomainChanges(ctx context.Context, bl, eu, id string, version uint64, now time.Time) error {
	s.mu.Lock()
	s.calls++
	fail := s.fail
	s.fail = false
	s.mu.Unlock()
	if fail {
		return errors.New("ack unavailable")
	}
	return s.Store.AckDomainChanges(ctx, bl, eu, id, version, now)
}

type receiveFailure struct{ task.Store }

func (s receiveFailure) Receive(context.Context, task.Source, []task.Spec, time.Time) error {
	return errors.New("application transaction failed")
}

func TestPersistentChanges(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			newTasks := func(t *testing.T) (task.Store, func() task.Store) {
				if backend == "memory" {
					s := taskmem.New()
					return s, func() task.Store { return s }
				}
				pool, dsn := pgtest.NewWithDSN(t)
				return taskpg.New(pool), func() task.Store {
					pool.Close()
					next, err := pg.Open(context.Background(), dsn, "")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(next.Close)
					return taskpg.New(next)
				}
			}
			t.Run("ack_loss_deduplicates_and_restart_recovers", func(t *testing.T) {
				ctx := context.Background()
				clock := newDeletionClock()
				books := &ackLoss{Store: memstore.New(clock.now), fail: true}
				if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error { return collectionFixture(b, "day", "p", clock.now()) }); err != nil {
					t.Fatal(err)
				}
				jobs, reopen := newTasks(t)
				k := &fakeKernel{}
				first := New(ctx, books, k, Options{Tasks: jobs, Now: clock.now})
				// 模拟已接收、ack 失败且尚未开始提交就退出。
				if err := first.receiveChanges(ctx, "bl", "u"); err == nil {
					t.Fatal("ack failure hidden")
				}
				finish(t, first)
				jobs = reopen()
				second := New(ctx, books, k, Options{Tasks: jobs, Now: clock.now})
				defer finish(t, second)
				if err := second.ScanTasks(ctx); err != nil {
					t.Fatal(err)
				}
				if err := second.ScanTasks(ctx); err != nil {
					t.Fatal(err)
				}
				if len(k.attempts) != 1 || len(k.sessions) != 1 {
					t.Fatal("ack replay duplicated command", len(k.attempts), len(k.sessions))
				}
				if pending, err := books.PendingDomainChanges(ctx, "bl", "u"); err != nil || len(pending) != 0 {
					t.Fatal(pending, err)
				}
				// 同一天合法重交产生新版本与新 input_id，不能被上一版任务吃掉。
				if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
					call := domain.NewCall(b, "day", []byte(`{"day":{"headline":"修订事务"}}`), nil, clock.now())
					call.FileDay()
					return call.Err()
				}); err != nil {
					t.Fatal(err)
				}
				if err := second.ScanTasks(ctx); err != nil {
					t.Fatal(err)
				}
				if len(k.attempts) != 2 || k.attempts[0].id == k.attempts[1].id || len(k.sessions) != 1 {
					t.Fatal("version identity lost")
				}
			})
			t.Run("receive_failure_never_acknowledges", func(t *testing.T) {
				ctx := context.Background()
				jobs, _ := newTasks(t)
				books := &ackLoss{Store: memstore.New(captureClock)}
				if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error { return collectionFixture(b, "day", "p", captureClock()) }); err != nil {
					t.Fatal(err)
				}
				o := New(ctx, books, &fakeKernel{}, Options{Tasks: receiveFailure{jobs}, Now: captureClock})
				defer finish(t, o)
				if err := o.ScanTasks(ctx); err == nil {
					t.Fatal("receive failure hidden")
				}
				if books.calls != 0 {
					t.Fatal("ack before commit")
				}
			})
			t.Run("rebuild_receipt_survives_local_commit_failure", func(t *testing.T) {
				ctx := context.Background()
				clock := newDeletionClock()
				jobs, reopen := newTasks(t)
				books := &rebuildCommitFailure{Store: memstore.New(clock.now), fail: true}
				eraseFixture(t, books, "2026-10-27", "erase", time.Time{})
				k := &deletionKernel{}
				first := New(ctx, books, k, Options{Tasks: jobs, Now: clock.now})
				if err := first.ScanDeletions(ctx); err == nil {
					t.Fatal("expected local failure")
				}
				finish(t, first)
				clock.advance()
				second := New(ctx, books, k, Options{Tasks: reopen(), Now: clock.now})
				defer finish(t, second)
				if err := second.ScanDeletions(ctx); err != nil {
					t.Fatal(err)
				}
				if len(k.attempts) != 1 || readTask(t, books).RebuildPending {
					t.Fatal("receipt not reused")
				}
			})
		})
	}
}

func TestTaskLoopStartupAndTick(t *testing.T) {
	ctx := context.Background()
	clock := newDeletionClock()
	books := memstore.New(clock.now)
	if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error { return collectionFixture(b, "day", "p", clock.now()) }); err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{started: make(chan struct{}, 1)}
	o := New(ctx, books, k, Options{Now: clock.now})
	defer finish(t, o)
	tick := make(chan time.Time, 1)
	o.StartTasks(tick)
	select {
	case <-k.started:
	case <-time.After(5 * time.Second):
		t.Fatal("startup scan missing")
	}
	// 等本轮提交完成，再创建第二个目标；tick 必须从持久变化发现它。
	deadline := time.After(5 * time.Second)
	for {
		k.mu.Lock()
		n := len(k.inputs)
		k.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("startup submission missing")
		case <-time.After(time.Millisecond):
		}
	}
	if err := books.Update(ctx, "bl", "other", func(b *domain.Book) error { return collectionFixture(b, "day2", "q", clock.now()) }); err != nil {
		t.Fatal(err)
	}
	tick <- clock.now()
	select {
	case <-k.started:
	case <-time.After(5 * time.Second):
		t.Fatal("periodic scan missing")
	}
	finish(t, o)
	if len(k.inputs) != 2 {
		t.Fatal("periodic task missing")
	}
}

func TestDayMappingKeepsLegacySessionsSeparate(t *testing.T) {
	ctx := context.Background()
	books := memstore.New(captureClock)
	if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{
			Projects:  []domain.Project{{ID: "p"}, {ID: "q"}},
			Exchanges: []domain.SeedExchange{{Date: "2026-10-27", Project: "p"}, {Date: "2026-10-28", Project: "q"}},
		}, captureClock())
		for _, sid := range []string{"seed_2026-10-27", "seed_2026-10-28"} {
			call := domain.NewCall(b, sid, []byte(`{"day":{"headline":"今日事务"}}`), nil, captureClock())
			call.FileDay()
			if err := call.Err(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changes, err := books.PendingDomainChanges(ctx, "bl", "u")
	if err != nil || len(changes) != 2 || changes[0].Date != changes[1].Date {
		t.Fatal(changes, err)
	}
	o := New(ctx, books, &fakeKernel{}, Options{Now: captureClock})
	defer finish(t, o)
	for i, change := range changes {
		specs, err := o.factTasks(ctx, change)
		if err != nil || len(specs) != 1 || specs[0].Target != []string{"project/p", "project/q"}[i] {
			t.Fatal(specs, err)
		}
	}
}

// Persona 已清理完成但应用结果未提交时，PendingDeletions 已不再发现 owner。
func TestApplicationScanSettlesCompletedDeletion(t *testing.T) {
	ctx := context.Background()
	clock := newDeletionClock()
	books := memstore.New(clock.now)
	d := eraseFixture(t, books, "2026-10-28", "erase", time.Time{})
	jobs := taskmem.New()
	source := deletionSource("bl", "u", d.ID, "delete")
	spec := task.Spec{Operation: "delete", Target: "deletions", Date: d.Date}
	if err := jobs.Receive(ctx, source, []task.Spec{spec}, clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error { return b.CompleteDeletionTask(d.ID, nil, clock.now()) }); err != nil {
		t.Fatal(err)
	}
	k := &deletionKernel{}
	o := New(ctx, books, k, Options{Tasks: jobs, Now: clock.now})
	defer finish(t, o)
	if err := o.ScanTasks(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, task.Identity(source, spec))
	if err != nil || got.State != task.Succeeded || len(k.calls) != 0 {
		t.Fatal(got.State, err, k.calls)
	}
}
