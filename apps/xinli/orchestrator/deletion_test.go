package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
	"persona/store/memstore"
	"xinli/kernel"
)

type deletionKernel struct {
	fakeKernel
	muDelete sync.Mutex
	calls    []string
	fail     string
	missing  bool
	noRecord bool
	cleaning bool
	active   bool
	deadline time.Time
	sources  []string
}

func (k *deletionKernel) record(step string) error {
	k.muDelete.Lock()
	defer k.muDelete.Unlock()
	k.calls = append(k.calls, step)
	if k.fail == step {
		return errors.New("private-content")
	}
	return nil
}
func (k *deletionKernel) Session(context.Context, string) (kernel.Session, error) {
	if err := k.record("session"); err != nil {
		return kernel.Session{}, err
	}
	k.muDelete.Lock()
	defer k.muDelete.Unlock()
	if k.missing {
		return kernel.Session{}, kernel.ErrNotFound
	}
	s := kernel.Session{}
	if k.active {
		s.Runs = []kernel.Run{{ID: "run", Status: "running"}}
	}
	return s, nil
}
func (k *deletionKernel) Interrupt(_ context.Context, _, run string) error {
	if run != "run" {
		return errors.New("wrong run")
	}
	return k.record("interrupt")
}
func (k *deletionKernel) DeleteSession(_ context.Context, _, reason string) error {
	if err := k.record("delete:" + reason); err != nil {
		return err
	}
	k.muDelete.Lock()
	defer k.muDelete.Unlock()
	if k.missing {
		return kernel.ErrNotFound
	}
	return nil
}
func (k *deletionKernel) Deletion(context.Context, string, string) (kernel.Deletion, error) {
	if err := k.record("deletion"); err != nil {
		return kernel.Deletion{}, err
	}
	k.muDelete.Lock()
	defer k.muDelete.Unlock()
	if k.noRecord || k.fail == "delete:erasure" {
		return kernel.Deletion{}, kernel.ErrNotFound
	}
	if k.cleaning {
		return kernel.Deletion{}, nil
	}
	return kernel.Deletion{CompletedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}, nil
}
func (k *deletionKernel) MemoriesBySource(_ context.Context, _, _ string, sessions []string) ([]kernel.Memory, error) {
	if err := k.record("memories"); err != nil {
		return nil, err
	}
	k.muDelete.Lock()
	defer k.muDelete.Unlock()
	k.sources = append([]string(nil), sessions...)
	return []kernel.Memory{{ID: "m", Content: "候选"}}, nil
}
func (k *deletionKernel) DeleteSessionMemories(_ context.Context, _, sid string) error {
	return k.record("delete_memories:" + sid)
}
func (k *deletionKernel) CreateSessionWithOptions(ctx context.Context, bl, eu, agent string, opts kernel.SessionOptions) (string, error) {
	if err := k.record("create"); err != nil {
		return "", err
	}
	k.muDelete.Lock()
	k.deadline = opts.DeleteAt
	k.muDelete.Unlock()
	return k.fakeKernel.CreateSessionWithOptions(ctx, bl, eu, agent, opts)
}
func (k *deletionKernel) Submit(ctx context.Context, sid, id, text string) error {
	if err := k.record("submit"); err != nil {
		return err
	}
	return k.fakeKernel.Submit(ctx, sid, id, text)
}

func (k *deletionKernel) SubmitInput(ctx context.Context, sid, id, text string) (kernel.InputReceipt, error) {
	if err := k.Submit(ctx, sid, id, text); err != nil {
		return kernel.InputReceipt{}, err
	}
	return kernel.InputReceipt{InputID: id, RunID: "run/" + id, Terminal: "completed"}, nil
}

type deletionClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *deletionClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *deletionClock) advance()       { c.mu.Lock(); c.at = c.at.Add(time.Hour); c.mu.Unlock() }
func newDeletionClock() *deletionClock {
	return &deletionClock{at: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func eraseFixture(t *testing.T, books store.Store, date, kind string, deadline time.Time) domain.DeletionTask {
	t.Helper()
	var task domain.DeletionTask
	err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		if _, err := b.AppendCaptures(date, "capture", []domain.CaptureSegment{{Speaker: "wearer", Text: "要擦除", TimeZone: "Asia/Shanghai"}, {Speaker: "wearer", Text: "保留", TimeZone: "Asia/Shanghai"}}); err != nil {
			return err
		}
		if err := b.BindDay(date, "old", deadline); err != nil {
			return err
		}
		if err := b.BindDay(date, "current", deadline); err != nil {
			return err
		}
		var err error
		if kind == "day" {
			task, err = b.EraseDay(date, captureClock())
		} else {
			task, err = b.Erase(date, "1.0", "1.0", captureClock())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func readTask(t *testing.T, books store.Store) domain.DeletionTask {
	t.Helper()
	var task domain.DeletionTask
	if err := books.View(context.Background(), "bl", "u", func(b *domain.Book) error { task = b.DeletionTasks()[0]; return nil }); err != nil {
		t.Fatal(err)
	}
	return task
}
func scan(t *testing.T, o *Orchestrator) {
	t.Helper()
	if err := o.ScanDeletions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEraseExecutionAndRebuild(t *testing.T) {
	c := newDeletionClock()
	books := memstore.New(captureClock)
	deadline, _ := domain.CaptureExpiresAt("2026-10-27", "Asia/Shanghai")
	eraseFixture(t, books, "2026-10-27", "erase", deadline)
	k := &deletionKernel{active: true}
	o := New(context.Background(), books, k, Options{Now: c.now})
	scan(t, o)
	scan(t, o)
	finish(t, o)
	if got := readTask(t, books); got.Status != "completed" || got.RebuildPending || len(got.Candidates) != 1 || got.Candidates[0].Summary != "候选" {
		t.Fatalf("task: %+v", got)
	}
	want := []string{"session", "interrupt", "delete:erasure", "deletion", "memories", "create", "submit"}
	if !reflect.DeepEqual(k.calls, want) {
		t.Fatalf("calls: %v", k.calls)
	}
	if !reflect.DeepEqual(k.sources, []string{"old", "current"}) || !k.deadline.Equal(deadline) {
		t.Fatal("lost sources or expiry")
	}
	if len(k.sessions) != 1 || k.sessions[0].agent != DayAgent || len(k.inputs) != 1 {
		t.Fatalf("created: %+v inputs: %+v", k.sessions, k.inputs)
	}
	for _, got := range k.inputs {
		if got.text != "整理今天" {
			t.Fatalf("input: %+v", got)
		}
	}
}

func TestDeletionMissingAndCleaning(t *testing.T) {
	for _, mode := range []string{"deleted", "absent", "cleaning"} {
		t.Run(mode, func(t *testing.T) {
			c := newDeletionClock()
			books := memstore.New(captureClock)
			eraseFixture(t, books, "2026-10-28", "erase", time.Time{})
			k := &deletionKernel{missing: true, noRecord: mode == "absent", cleaning: mode == "cleaning"}
			o := New(context.Background(), books, k, Options{Now: c.now})
			scan(t, o)
			if mode == "cleaning" {
				if got := readTask(t, books); got.Status != "pending" || got.Error != "" {
					t.Fatalf("task: %+v", got)
				}
				if len(k.sources) != 0 {
					t.Fatal("queried before cleanup")
				}
				c.advance()
				k.cleaning = false
				scan(t, o)
			}
			finish(t, o)
			if got := readTask(t, books); got.Status != "completed" {
				t.Fatalf("task: %+v", got)
			}
			if len(k.sessions) != 0 {
				t.Fatal("today rebuilt")
			}
		})
	}
}

func TestDeletionFailureRetry(t *testing.T) {
	for step, class := range map[string]string{"session": "session", "interrupt": "interrupt", "delete:erasure": "delete_session", "deletion": "deletion", "memories": "memories", "create": "rebuild", "submit": "submit"} {
		t.Run(step, func(t *testing.T) {
			c := newDeletionClock()
			books := memstore.New(captureClock)
			eraseFixture(t, books, "2026-10-27", "erase", time.Time{})
			k := &deletionKernel{active: true, fail: step}
			o := New(context.Background(), books, k, Options{Now: c.now})
			if err := o.ScanDeletions(context.Background()); err == nil {
				t.Fatal("missing failure")
			}
			if task := readTask(t, books); task.Error != class || task.FailedAt.IsZero() {
				t.Fatalf("task: %+v", task)
			}
			count := len(k.calls)
			scan(t, o)
			if len(k.calls) != count {
				t.Fatal("backoff ignored")
			}
			k.fail = ""
			c.advance()
			scan(t, o)
			finish(t, o)
			if task := readTask(t, books); task.Status != "completed" || task.RebuildPending || task.Error != "" {
				t.Fatalf("task: %+v", task)
			}
			wantSessions := 1
			if step == "submit" {
				wantSessions = 2
			}
			if len(k.sessions) != wantSessions || len(k.inputs) != 1 {
				t.Fatal("rebuild did not isolate unknown submission")
			}
		})
	}
}

func TestDeletionDayAndNoRebuild(t *testing.T) {
	for _, mode := range []string{"day", "today", "expired", "timezone"} {
		t.Run(mode, func(t *testing.T) {
			c := newDeletionClock()
			books := memstore.New(captureClock)
			kind, date := "erase", "2026-10-27"
			deadline := time.Time{}
			if mode == "day" {
				kind = "day"
			}
			if mode == "today" {
				date = "2026-10-28"
			}
			if mode == "expired" {
				c.at, _ = domain.CaptureExpiresAt(date, "Asia/Shanghai")
			}
			if mode == "timezone" {
				c.at = time.Date(2026, 10, 27, 16, 30, 0, 0, time.UTC)
				date = "2026-10-28"
			}
			eraseFixture(t, books, date, kind, deadline)
			k := &deletionKernel{}
			o := New(context.Background(), books, k, Options{Now: c.now, DefaultTimeZone: "UTC"})
			scan(t, o)
			finish(t, o)
			if len(k.sessions) != 0 || len(k.inputs) != 0 {
				t.Fatal("unexpected rebuild")
			}
			task := readTask(t, books)
			if task.Status != "completed" {
				t.Fatalf("task: %+v", task)
			}
			if mode == "day" {
				if len(task.Candidates) != 0 || !reflect.DeepEqual(k.calls, []string{"session", "delete:", "deletion", "delete_memories:old", "delete_memories:current"}) {
					t.Fatalf("day: %+v %v", task, k.calls)
				}
			}
		})
	}
}

func TestEnsureDaySession(t *testing.T) {
	c := newDeletionClock()
	books := memstore.New(captureClock)
	k := &deletionKernel{}
	deadline, _ := domain.CaptureExpiresAt("2026-10-28", "Asia/Shanghai")
	// 擦除释放绑定但保留期限；无内核 Session 的待办也可完成。
	task := eraseFixture(t, books, "2026-10-28", "erase", deadline)
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error { return b.CompleteDeletionTask(task.ID, nil, c.now()) }); err != nil {
		t.Fatal(err)
	}
	o := New(context.Background(), books, k, Options{Now: c.now})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sid, err := o.EnsureDaySession(context.Background(), "bl", "u", "2026-10-28")
			if err != nil || sid != "ses_1" {
				t.Errorf("ensure: %s %v", sid, err)
			}
		}()
	}
	wg.Wait()
	if !k.deadline.Equal(deadline) || len(k.sessions) != 1 {
		t.Fatal("binding or expiry not reused")
	}
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error { _, err := b.EraseDay("2026-10-28", c.now()); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := o.EnsureDaySession(context.Background(), "bl", "u", "2026-10-28"); !errors.Is(err, domain.ErrDayDeleted) {
		t.Fatalf("deleted day: %v", err)
	}
	finish(t, o)
}

func TestStartupRecoversRebuildWithRetirement(t *testing.T) {
	c := newDeletionClock()
	books := memstore.New(captureClock)
	eraseFixture(t, books, "2026-10-27", "erase", time.Time{})
	k := &deletionKernel{fail: "submit"}
	first := New(context.Background(), books, k, Options{Now: c.now})
	if err := first.ScanDeletions(context.Background()); err == nil {
		t.Fatal("missing submit failure")
	}
	finish(t, first)
	if task := readTask(t, books); task.Status != "completed" || !task.RebuildPending {
		t.Fatalf("task: %+v", task)
	}
	k.fail = ""
	c.advance()
	second := New(context.Background(), books, k, Options{Now: c.now, RetryInterval: time.Millisecond, Tasks: first.tasks})
	defer finish(t, second)
	second.StartDeletions()
	deadline := time.After(5 * time.Second)
	for readTask(t, books).RebuildPending {
		select {
		case <-deadline:
			t.Fatal("startup did not resume")
		case <-time.After(time.Millisecond):
		}
	}
	finish(t, second)
	if len(k.sessions) != 2 || len(k.inputs) != 1 {
		t.Fatal("startup did not isolate unknown input")
	}
	third := New(context.Background(), books, k, Options{Now: c.now, Tasks: first.tasks})
	scan(t, third)
	finish(t, third)
	if len(k.sessions) != 2 || len(k.inputs) != 1 {
		t.Fatal("completed work repeated")
	}
}

func TestStartupPendingAndNotificationShutdown(t *testing.T) {
	c := newDeletionClock()
	books := memstore.New(captureClock)
	eraseFixture(t, books, "2026-10-28", "erase", time.Time{})
	k := &deletionKernel{cleaning: true}
	first := New(context.Background(), books, k, Options{Now: c.now})
	scan(t, first)
	finish(t, first)
	k.cleaning = false
	c.advance()
	second := New(context.Background(), books, k, Options{Now: c.now, RetryInterval: time.Millisecond, Tasks: first.tasks})
	request, cancel := context.WithCancel(context.Background())
	cancel()
	second.DeletionPending(request, "bl", "u", "ignored")
	defer finish(t, second)
	second.StartDeletions()
	deadline := time.After(5 * time.Second)
	for readTask(t, books).Status != "completed" {
		select {
		case <-deadline:
			t.Fatal("pending not recovered")
		case <-time.After(time.Millisecond):
		}
	}
	finish(t, second)
	count := len(k.calls)
	second.DeletionPending(context.Background(), "bl", "u", "late")
	second.StartDeletions()
	finish(t, second)
	if len(k.calls) != count {
		t.Fatal("work accepted after shutdown")
	}
}

// 模拟内核已接受输入、但本地还没记下整理完成时进程退出。
type rebuildCommitFailure struct {
	store.Store
	fail bool
}

func (s *rebuildCommitFailure) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		before := b.PendingDeletionTasks()
		if err := fn(b); err != nil {
			return err
		}
		if s.fail && len(before) > 0 && before[0].RebuildPending && len(b.PendingDeletionTasks()) == 0 {
			s.fail = false
			return errors.New("commit failed")
		}
		return nil
	})
}
func TestRebuildCommitCrashDeduplicatesAcceptedInput(t *testing.T) {
	c := newDeletionClock()
	books := &rebuildCommitFailure{Store: memstore.New(captureClock), fail: true}
	eraseFixture(t, books, "2026-10-27", "erase", time.Time{})
	k := &deletionKernel{}
	first := New(context.Background(), books, k, Options{Now: c.now})
	if err := first.ScanDeletions(context.Background()); err == nil {
		t.Fatal("missing commit failure")
	}
	finish(t, first)
	c.advance()
	second := New(context.Background(), books, k, Options{Now: c.now, Tasks: first.tasks})
	scan(t, second)
	finish(t, second)
	if len(k.sessions) != 1 || len(k.attempts) != 1 || len(k.inputs) != 1 {
		t.Fatalf("sessions=%d attempts=%d inputs=%d", len(k.sessions), len(k.attempts), len(k.inputs))
	}
	if task := readTask(t, books); task.RebuildPending || task.Error != "" {
		t.Fatalf("task: %+v", task)
	}
}

func TestDayMemoryFailureMustNotComplete(t *testing.T) {
	c := newDeletionClock()
	books := memstore.New(captureClock)
	eraseFixture(t, books, "2026-10-27", "day", time.Time{})
	k := &deletionKernel{fail: "delete_memories:old"}
	o := New(context.Background(), books, k, Options{Now: c.now})
	if err := o.ScanDeletions(context.Background()); err == nil {
		t.Fatal("missing memory failure")
	}
	if task := readTask(t, books); task.Status != "pending" || task.Error != "delete_memories" {
		t.Fatalf("task: %+v", task)
	}
	if k.calls[len(k.calls)-1] != "delete_memories:old" {
		t.Fatal("continued after failure")
	}
	k.fail = ""
	c.advance()
	scan(t, o)
	finish(t, o)
	if readTask(t, books).Status != "completed" {
		t.Fatal("retry failed")
	}
}

func TestSessionlessDeletionAndDefaultTimezone(t *testing.T) {
	c := newDeletionClock()
	c.at = time.Date(2026, 10, 27, 16, 30, 0, 0, time.UTC)
	books := memstore.New(captureClock)
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		if _, err := b.AppendCaptures("2026-10-28", "c", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文"}}); err != nil {
			return err
		}
		_, err := b.Erase("2026-10-28", "1.0", "1.0", c.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	k := &deletionKernel{}
	o := New(context.Background(), books, k, Options{Now: c.now})
	scan(t, o)
	finish(t, o)
	if !reflect.DeepEqual(k.calls, []string{"memories"}) {
		t.Fatalf("sessionless calls: %v", k.calls)
	}
}

func TestPeriodicRetryAndOrderedTasks(t *testing.T) {
	c := newDeletionClock()
	books := memstore.New(captureClock)
	eraseFixture(t, books, "2026-10-28", "erase", time.Time{})
	k := &deletionKernel{cleaning: true}
	o := New(context.Background(), books, k, Options{Now: c.now, RetryInterval: time.Millisecond, RetryMax: time.Second})
	scan(t, o)
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		_, err := b.Erase("2026-10-28", "1.1", "1.1", c.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// 前一条在退避时，后一条即使没有 Session 也不能提前查询候选。
	count := len(k.calls)
	scan(t, o)
	if len(k.calls) != count {
		t.Fatal("later task bypassed pending cleanup")
	}
	k.muDelete.Lock()
	k.cleaning = false
	k.muDelete.Unlock()
	defer finish(t, o)
	o.StartDeletions()
	c.advance()
	deadline := time.After(5 * time.Second)
	for {
		var pending int
		if err := books.View(context.Background(), "bl", "u", func(b *domain.Book) error { pending = len(b.PendingDeletionTasks()); return nil }); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("periodic retry did not finish")
		case <-time.After(time.Millisecond):
		}
	}
	finish(t, o)
}

func TestLaterDayDeletionCancelsRebuildIntent(t *testing.T) {
	c := newDeletionClock()
	books := memstore.New(captureClock)
	eraseFixture(t, books, "2026-10-27", "erase", time.Time{})
	k := &deletionKernel{fail: "submit"}
	o := New(context.Background(), books, k, Options{Now: c.now})
	if err := o.ScanDeletions(context.Background()); err == nil {
		t.Fatal("missing submit failure")
	}
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error { _, err := b.EraseDay("2026-10-27", c.now()); return err }); err != nil {
		t.Fatal(err)
	}
	k.fail = ""
	c.advance()
	scan(t, o)
	finish(t, o)
	if len(k.sessions) != 1 || len(k.inputs) != 0 {
		t.Fatal("recreated deleted day")
	}
}
