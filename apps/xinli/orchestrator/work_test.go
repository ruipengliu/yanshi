package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"persona/domain"
	personapg "persona/pg"
	personatest "persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	bookpg "persona/store/pgstore"
	"xinli/kernel"
	"xinli/pg"
	"xinli/pg/pgtest"
	"xinli/task"
	taskmem "xinli/task/memstore"
	taskpg "xinli/task/pgstore"
)

func workStores(t *testing.T, backend string, now func() time.Time) (store.Store, task.Store, func() (store.Store, task.Store)) {
	t.Helper()
	if backend == "memory" {
		books, jobs := memstore.New(now), taskmem.New()
		return books, jobs, func() (store.Store, task.Store) {
			return books, jobs
		}
	}
	bp, bd := personatest.NewWithDSN(t)
	jp, jd := pgtest.NewWithDSN(t)
	return bookpg.New(bp, now), taskpg.New(jp), func() (store.Store, task.Store) {
		bp.Close()
		jp.Close()
		b, err := personapg.Open(context.Background(), bd, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		j, err := pg.Open(context.Background(), jd, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(j.Close)
		return bookpg.New(b, now), taskpg.New(j)
	}
}

type workKernel struct {
	deletionKernel
	createdAt time.Time
	terminal  string
	lost      bool
	readFail  bool
	unsafe    bool
	deleted   []string
}

func (k *workKernel) Sessions(_ context.Context, bl, eu string) ([]kernel.SessionInfo, error) {
	out := []kernel.SessionInfo{}
	for _, s := range k.sessions {
		if s.bl == bl && s.eu == eu {
			out = append(out, kernel.SessionInfo{ID: s.sid, EndUser: eu, Agent: s.agent, CreatedAt: k.createdAt})
		}
	}
	return out, nil
}
func (k *workKernel) EmptySession(_ context.Context, sid string) (bool, error) {
	for _, input := range k.inputs {
		if input.sid == sid {
			return false, nil
		}
	}
	return true, nil
}
func (k *workKernel) DeleteSession(_ context.Context, sid, reason string) error {
	if reason != "" {
		return errors.New("wrong orphan reason")
	}
	k.deleted = append(k.deleted, sid)
	return nil
}
func (k *workKernel) SubmitInput(ctx context.Context, sid, id, text string) (kernel.InputReceipt, error) {
	if err := k.fakeKernel.Submit(ctx, sid, id, text); err != nil {
		return kernel.InputReceipt{}, err
	}
	if k.lost {
		return kernel.InputReceipt{}, errors.New("lost response")
	}
	return kernel.InputReceipt{InputID: id, RunID: "run/" + sid, Terminal: k.terminal, Unsafe: k.unsafe}, nil
}
func (k *workKernel) FindInput(_ context.Context, sid, id string) (kernel.InputReceipt, bool, error) {
	if k.readFail {
		return kernel.InputReceipt{}, false, errors.New("read unavailable")
	}
	_, found := k.inputs[sid+"/"+id]
	return kernel.InputReceipt{InputID: id, RunID: "run/" + sid, Terminal: k.terminal, Unsafe: k.unsafe}, found, nil
}

type failBinding struct {
	store.Store
	fail bool
}

func (s *failBinding) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		before := b.BusinessDay("2026-10-28").Session
		if err := fn(b); err != nil {
			return err
		}
		if s.fail && before == "" && b.BusinessDay("2026-10-28").Session != "" {
			s.fail = false
			return errors.New("exit before bind commit")
		}
		return nil
	})
}

type loseCreated struct {
	task.Store
	fail bool
}

func (s *loseCreated) RecordCreated(ctx context.Context, id, sid string) error {
	if s.fail {
		s.fail = false
		return errors.New("exit before recording response")
	}
	return s.Store.RecordCreated(ctx, id, sid)
}

func seedWorkDay(t *testing.T, s store.Store) {
	t.Helper()
	if err := s.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		_, err := b.AppendCaptures("2026-10-28", "c", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文", TimeZone: "UTC"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkCreationRecovery(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		for _, lostID := range []bool{false, true} {
			t.Run(backend+map[bool]string{false: "/before_bind", true: "/before_record"}[lostID], func(t *testing.T) {
				ctx := context.Background()
				clock := newDeletionClock()
				books, jobs, reopen := workStores(t, backend, clock.now)
				seedWorkDay(t, books)
				k := &workKernel{createdAt: clock.now(), terminal: "completed"}
				var firstBooks store.Store = books
				var firstJobs task.Store = jobs
				if lostID {
					firstJobs = &loseCreated{Store: jobs, fail: true}
				} else {
					firstBooks = &failBinding{Store: books, fail: true}
				}
				first := New(ctx, firstBooks, k, Options{Now: clock.now, Tasks: firstJobs})
				if _, err := first.DayInput(ctx, "bl", "u", "2026-10-28", "day", "one"); err == nil {
					t.Fatal("failure hidden")
				}
				finish(t, first)
				if len(k.inputs) != 0 || len(k.sessions) != 1 {
					t.Fatal("content before binding")
				}
				clock.advance()
				books, jobs = reopen()
				second := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
				defer finish(t, second)
				if err := second.ScanOrphans(ctx); err != nil {
					t.Fatal(err)
				}
				if len(k.deleted) != 1 || k.deleted[0] != "ses_1" {
					t.Fatal("orphan not deleted", k.deleted)
				}
				if sid, err := second.DayInput(ctx, "bl", "u", "2026-10-28", "day", "one"); err != nil || sid != "ses_2" {
					t.Fatal(sid, err)
				}
				if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
					if b.BusinessDay("2026-10-28").Session != "ses_2" {
						t.Fatal("missing binding")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				intents, err := jobs.Creations(ctx, clock.now().Add(time.Hour), 256)
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if lostID {
					want = 1
				}
				if len(intents) != want {
					t.Fatal("creation recovery record", len(intents))
				}
			})
		}
	}
}

func TestWorkCommandRetirement(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		for _, mode := range []string{"timeout", "interrupted", "unknown", "unsafe", "failed", "takeover"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				clock := newDeletionClock()
				books, jobs, reopen := workStores(t, backend, clock.now)
				seedWorkDay(t, books)
				k := &workKernel{createdAt: clock.now()}
				first := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
				old, err := first.DayInput(ctx, "bl", "u", "2026-10-28", "exchange", "one")
				if err != nil {
					t.Fatal(err)
				}
				deadline := k.deadline
				if _, err := first.DayInput(ctx, "bl", "u", "2026-10-28", "day", "two"); !errors.Is(err, domain.ErrWorkBusy) {
					t.Fatal("second command did not wait", err)
				}
				if len(k.inputs) != 1 {
					t.Fatal("overlapping commands")
				}
				if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
					c := domain.NewCall(b, old, []byte(`{}`), nil, clock.now())
					if got := c.FileDay(); got != domain.ErrWorkCommand.Error() {
						t.Fatal("type guard", got)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				finish(t, first)
				clock.mu.Lock()
				if mode == "timeout" {
					clock.at = clock.at.Add(time.Hour)
				} else {
					clock.at = clock.at.Add(time.Minute)
				}
				clock.mu.Unlock()
				books, jobs = reopen()
				second := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
				defer finish(t, second)
				switch mode {
				case "interrupted":
					k.terminal = "interrupted"
				case "failed":
					k.terminal = "failed"
				case "unknown":
					k.readFail = true
				case "takeover":
					k.unsafe = true
				case "unsafe":
					k.terminal = "completed"
					k.unsafe = true
				}
				if err := second.ScanTasks(ctx); err != nil {
					t.Fatal(err)
				}
				if _, held, err := jobs.SessionTask(ctx, task.Owner{BusinessLine: "bl", EndUser: "u"}, old); err != nil || held {
					t.Fatal("retired session still holds command slot", held, err)
				}
				k.readFail, k.unsafe, k.terminal = false, false, "completed"
				clock.mu.Lock()
				clock.at = clock.at.Add(time.Minute)
				clock.mu.Unlock()
				if err := second.ScanTasks(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := second.DayInput(ctx, "bl", "u", "2026-10-28", "day", "two"); err != nil {
					t.Fatal(err)
				}
				if len(k.sessions) != 2 || !k.deadline.Equal(deadline) {
					t.Fatal("retirement binding/deadline")
				}
				if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
					c := domain.NewCall(b, old, []byte(`{"day":{"headline":"迟到"}}`), nil, clock.now())
					if got := c.FileDay(); got != domain.ErrWorkRetired.Error() {
						t.Fatal("late write", got)
					}
					if _, err := c.Captures(domain.PageOptions{}); !errors.Is(err, domain.ErrWorkRetired) {
						t.Fatal("late read", err)
					}
					if len(b.State().Days) != 0 {
						t.Fatal("late write committed")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestErasureDeletesRetiredWorkSessions(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := newDeletionClock()
			books, jobs, _ := workStores(t, backend, clock.now)
			seedWorkDay(t, books)
			k := &deletionKernel{}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			old, err := o.DayInput(ctx, "bl", "u", "2026-10-28", "day", "one")
			if err != nil {
				t.Fatal(err)
			}
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				return b.RetireWork("day/2026-10-28", old, clock.now())
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := o.DayInput(ctx, "bl", "u", "2026-10-28", "day", "two"); err != nil {
				t.Fatal(err)
			}
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				_, err := b.Erase("2026-10-28", "1.0", "1.0", clock.now())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := o.ScanDeletions(ctx); err != nil {
				t.Fatal(err)
			}
			deletes := 0
			for _, step := range k.calls {
				if step == "delete:erasure" {
					deletes++
				}
			}
			if deletes != 2 {
				t.Fatal("retired raw copy survived", k.calls)
			}
		})
	}
}

func TestLegacyFailedSlotRetiresBeforeNextCommand(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := newDeletionClock()
			books, jobs, _ := workStores(t, backend, clock.now)
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}}}, clock.now())
				b.Bind("p", "legacy")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			source := task.Source{Owner: task.Owner{BusinessLine: "bl", EndUser: "u"}, Kind: "legacy", ID: "legacy", Version: 1}
			spec := task.Spec{Operation: "collect", Target: "project/p", Date: "2026-10-28"}
			if err := jobs.Receive(ctx, source, []task.Spec{spec}, clock.now()); err != nil {
				t.Fatal(err)
			}
			old, ok, err := jobs.Claim(ctx, task.Identity(source, spec), clock.now(), time.Minute)
			if err != nil || !ok {
				t.Fatal(ok, err)
			}
			old.Session, old.State = "legacy", task.Failed
			old.Receipt = kernel.InputReceipt{InputID: old.InputID, RunID: "old-run", Terminal: "interrupted"}
			if err := jobs.Save(ctx, old, clock.now(), true); err != nil {
				t.Fatal(err)
			}
			k := &fakeKernel{}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			job, err := o.weekInput(ctx, "bl", "u", "2026-10-22", "rewrite", "project/p")
			if err != nil || job.State != task.Succeeded || job.Session == "legacy" {
				t.Fatal(job.State, job.Session, err)
			}
			if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
				if !errors.Is(b.CheckWorkSession("legacy"), domain.ErrWorkRetired) {
					t.Fatal("legacy late write enabled")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
