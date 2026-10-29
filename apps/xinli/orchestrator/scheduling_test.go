package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
	"xinli/kernel"
	"xinli/schedule"
	"xinli/task"
)

type scheduleClock struct {
	at time.Time
}

func (c *scheduleClock) now() time.Time {
	return c.at
}

func (c *scheduleClock) set(s string) {
	c.at, _ = time.Parse(time.RFC3339, s)
}

type scheduleKernel struct {
	workKernel
	books          store.Store
	now            func() time.Time
	progress, file bool
}

func (k *scheduleKernel) SubmitInput(ctx context.Context, sid, id, text string) (kernel.InputReceipt, error) {
	r, err := k.workKernel.SubmitInput(ctx, sid, id, text)
	if err != nil {
		return r, err
	}
	if text != "整理今天" && text != "回顾本周" {
		return r, nil
	}
	err = k.books.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if text == "回顾本周" {
			if k.file {
				c := domain.NewCall(b, sid, []byte(`{"habits":[],"suggestions":[]}`), nil, k.now())
				c.FileWeek()
				if !c.WeekFiled {
					return fmt.Errorf("week rejected: %v", c.Err())
				}
			}
			return nil
		}
		if k.progress {
			for range 20 {
				c := domain.NewCall(b, sid, []byte(`{"after":"new","all":true}`), nil, k.now())
				p, err := c.Captures(domain.PageOptions{Limit: 200, MaxChars: 6000, GapMS: 300000, MaxToolResult: 8000})
				if err != nil {
					return err
				}
				if len(p.Segments) == 0 {
					break
				}
				args, _ := json.Marshal(map[string]any{"source": map[string]string{"from": "#" + p.Segments[0].Ref, "to": "#" + p.Segments[len(p.Segments)-1].Ref}})
				c = domain.NewCall(b, sid, args, nil, k.now())
				c.FileChanges()
				if c.Err() != nil {
					return c.Err()
				}
			}
		}
		if k.file {
			c := domain.NewCall(b, sid, []byte(`{"day":{"headline":"今日事务"}}`), nil, k.now())
			c.FileDay()
			if c.Filed == nil {
				return fmt.Errorf("day rejected: %v", c.Err())
			}
		}
		return nil
	})
	return r, err
}

func appendScheduled(t *testing.T, books store.Store, date, batch, zone string, now time.Time) {
	t.Helper()
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		_, err := b.AppendCaptures(date, batch, []domain.CaptureSegment{{Speaker: "wearer", Text: "一段原文", SpokenAt: now.Format(time.RFC3339), TimeZone: zone}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func scheduledJob(t *testing.T, jobs task.Store, kind, date string, revision uint64) task.Task {
	t.Helper()
	in := scheduled(task.Owner{BusinessLine: "bl", EndUser: "u"}, kind, date, revision)
	job, err := jobs.Get(context.Background(), task.Identity(in.source, in.spec))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestAutomaticDayAndMidnight(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-28T09:00:00Z")
			books, jobs, reopen := workStores(t, backend, clock.now)
			k := &scheduleKernel{books: books, now: clock.now, progress: true, file: true, workKernel: workKernel{terminal: "completed"}}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			appendScheduled(t, books, "2026-10-28", "one", "UTC", clock.now())
			clock.set("2026-10-28T09:04:59Z")
			if err := o.ScanSchedules(ctx); err != nil || len(k.inputs) != 0 {
				t.Fatal(err, len(k.inputs))
			}
			clock.set("2026-10-28T09:05:00Z")
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if job := scheduledJob(t, jobs, "day", "2026-10-28", 1); job.State != task.Succeeded {
				t.Fatal(job)
			}
			clock.set("2026-10-28T12:00:00Z")
			appendScheduled(t, books, "2026-10-28", "two", "Asia/Shanghai", clock.now())
			clock.set("2026-10-28T12:05:00Z")
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if len(k.inputs) != 2 || len(k.sessions) != 1 {
				t.Fatal("multiple silences did not reuse day", len(k.inputs), len(k.sessions))
			}
			clock.set("2026-10-28T18:00:00Z")
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if job := scheduledJob(t, jobs, "final", "2026-10-28", 2); job.State != task.Succeeded {
				t.Fatal(job)
			}
			finish(t, o)
			books, jobs = reopen()
			k.books = books
			o = New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			if err := o.ScanSchedules(ctx); err != nil || len(k.inputs) != 3 {
				t.Fatal("restart duplicated work", err, len(k.inputs))
			}
			clock.set("2026-10-28T23:59:00Z")
			appendScheduled(t, books, "2026-10-28", "three", "UTC", clock.now())
			clock.set("2026-10-29T00:01:00Z")
			appendScheduled(t, books, "2026-10-29", "one", "UTC", clock.now())
			clock.set("2026-10-29T00:06:00Z")
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			clock.set("2026-10-29T01:59:00Z")
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if job := scheduledJob(t, jobs, "final", "2026-10-28", 3); job.State != task.Succeeded {
				t.Fatal(job)
			}
			if len(k.sessions) != 2 || len(k.inputs) != 5 {
				t.Fatal("midnight mixed business days", len(k.sessions), len(k.inputs))
			}
		})
	}
}

func TestForcedFinalStableAcrossRestart(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-29T03:59:00+08:00")
			books, jobs, reopen := workStores(t, backend, clock.now)
			appendScheduled(t, books, "2026-10-28", "late", "Asia/Shanghai", clock.now())
			k := &scheduleKernel{books: books, now: clock.now, progress: true, file: true, workKernel: workKernel{terminal: "completed"}}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			if err := o.ScanSchedules(ctx); err != nil || len(k.inputs) != 0 {
				t.Fatal(err, len(k.inputs))
			}
			finish(t, o)
			books, jobs = reopen()
			k.books = books
			o = New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			z, _ := time.LoadLocation("Asia/Shanghai")
			due := time.Date(2026, 10, 29, 4, 0, 0, 0, z).Add(schedule.Jitter("bl", "u", "2026-10-28"))
			clock.at = due.Add(-time.Second)
			if err := o.ScanSchedules(ctx); err != nil || len(k.inputs) != 0 {
				t.Fatal(err, len(k.inputs))
			}
			clock.at = due
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if job := scheduledJob(t, jobs, "final", "2026-10-28", 1); job.State != task.Succeeded {
				t.Fatal(job)
			}
		})
	}
}

func TestScheduleRequiresAcceptanceAndProgress(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		for _, mode := range []string{"neither", "fact_only", "progress_only"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				clock := &scheduleClock{}
				clock.set("2026-10-28T09:00:00Z")
				books, jobs, reopen := workStores(t, backend, clock.now)
				appendScheduled(t, books, "2026-10-28", "one", "UTC", clock.now())
				k := &scheduleKernel{books: books, now: clock.now, file: mode == "fact_only", progress: mode == "progress_only", workKernel: workKernel{terminal: "completed"}}
				o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
				clock.at = clock.at.Add(5 * time.Minute)
				if err := o.ScanSchedules(ctx); err != nil {
					t.Fatal(err)
				}
				job := scheduledJob(t, jobs, "day", "2026-10-28", 1)
				if job.State != task.Pending || job.Retirements != 1 || !job.RetryAt.After(clock.now()) {
					t.Fatal(job)
				}
				finish(t, o)
				books, jobs = reopen()
				k.books = books
				o = New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
				defer finish(t, o)
				for i := 1; i < schedule.MaxAttempts; i++ {
					clock.at = job.RetryAt.Add(-time.Microsecond)
					if err := o.ScanTasks(ctx); err != nil || len(k.inputs) != i {
						t.Fatal("retry before due", err, len(k.inputs))
					}
					clock.at = job.RetryAt
					err := o.ScanTasks(ctx)
					if err != nil && !errors.Is(err, task.ErrNoProgress) {
						t.Fatal(err)
					}
					job = scheduledJob(t, jobs, "day", "2026-10-28", 1)
				}
				if job.State != task.Failed || job.LastErrorClass != "no_progress" || len(k.inputs) != schedule.MaxAttempts {
					t.Fatal(job, len(k.inputs))
				}
				if n, err := jobs.FailureCount(ctx, job.Owner); err != nil || n != 1 {
					t.Fatal(n, err)
				}
				if err := o.ScanTasks(ctx); err != nil || len(k.inputs) != schedule.MaxAttempts {
					t.Fatal(err, len(k.inputs))
				}
			})
		}
	}
}

func TestScheduleExpiryAndOwnerBarrier(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-28T09:00:00Z")
			books, jobs, _ := workStores(t, backend, clock.now)
			appendScheduled(t, books, "2026-10-28", "one", "UTC", clock.now())
			k := &workKernel{}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			clock.at = clock.at.Add(5 * time.Minute)
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			expires, _ := domain.CaptureExpiresAt("2026-10-28", "UTC")
			clock.at = expires
			if err := o.ScanTasks(ctx); !errors.Is(err, task.ErrExpired) {
				t.Fatal(err)
			}
			if err := o.ScanExpirations(ctx); err != nil {
				t.Fatal(err)
			}
			job := scheduledJob(t, jobs, "day", "2026-10-28", 1)
			if job.State != task.Failed || job.LastErrorClass != "expired_unprocessed" || !k.deadline.Equal(expires) {
				t.Fatal(job, k.deadline)
			}
			if _, err := books.BeginOwnerDeletion(ctx, "bl", "u", clock.now()); err != nil {
				t.Fatal(err)
			}
			if owners, err := books.SchedulingOwners(ctx); err != nil || len(owners) != 0 {
				t.Fatal(owners, err)
			}
			if err := o.ScanSchedules(ctx); err != nil || len(k.inputs) != 1 {
				t.Fatal(err, len(k.inputs))
			}
		})
	}
}

func TestWeekAcceptedFanoutAndReplay(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-11-01T23:59:00Z")
			books, jobs, reopen := workStores(t, backend, clock.now)
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}, {ID: "future"}, {ID: "old"}}, Exchanges: []domain.SeedExchange{
					{Date: "2026-10-26", Project: "p"}, {Date: "2026-11-02", Project: "future"}, {Date: "2026-10-25", Project: "old"},
				}}, clock.now())
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			k := &workKernel{}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			sid, err := o.Week(ctx, "bl", "u")
			if err != nil {
				t.Fatal(err)
			}
			if err := o.ScanTasks(ctx); err != nil || len(k.inputs) != 1 {
				t.Fatal("rewrite before acceptance", err, len(k.inputs))
			}
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				c := domain.NewCall(b, sid, []byte(`{"habits":[],"suggestions":[]}`), nil, clock.now())
				c.FileWeek()
				if !c.WeekFiled {
					t.Fatal("empty week rejected")
				}
				return c.Err()
			}); err != nil {
				t.Fatal(err)
			}
			finish(t, o)
			books, jobs = reopen()
			loss := &ackLoss{Store: books, fail: true}
			o = New(ctx, loss, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			if err := o.receiveChanges(ctx, "bl", "u"); err == nil {
				t.Fatal("ack loss hidden")
			}
			for range 2 {
				if err := o.ScanTasks(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if len(k.inputs) != 2 || len(k.sessions) != 2 || k.sessions[1].agent != ProjectAgent {
				t.Fatal("week fanout duplicated or omitted unbound project", len(k.inputs), k.sessions)
			}
			if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
				for _, p := range b.State().Projects {
					if p.ID == "p" && p.Session == "" || p.ID != "p" && p.Session != "" {
						t.Fatal("wrong week project", p.ID)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if pending, err := books.PendingDomainChanges(ctx, "bl", "u"); err != nil || len(pending) != 0 {
				t.Fatal(pending, err)
			}
		})
	}
}

// 双实例只竞争持久任务，不靠进程内锁决定是否提交。
func TestConcurrentScheduleDiscovery(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-28T09:00:00Z")
			books, jobs, _ := workStores(t, backend, clock.now)
			appendScheduled(t, books, "2026-10-28", "one", "UTC", clock.now())
			clock.at = clock.at.Add(5 * time.Minute)
			k := &deletionKernel{}
			one, two := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs}), New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, one)
			defer finish(t, two)
			var wg sync.WaitGroup
			for _, o := range []*Orchestrator{one, two} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := o.ScanSchedules(ctx); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if len(k.inputs) != 1 {
				t.Fatal("duplicate input", len(k.inputs))
			}
		})
	}
}

func TestBoundWeekReadsFixedWindow(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-11-05T12:00:00Z")
			books, _, _ := workStores(t, backend, clock.now)
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				for _, date := range []string{"2026-10-26", "2026-11-01", "2026-11-02"} {
					if err := b.BindDay(date, date); err != nil {
						return err
					}
					at, _ := time.Parse(time.DateOnly, date)
					c := domain.NewCall(b, date, []byte(`{"day":{"headline":"今日事务"}}`), nil, at)
					c.FileDay()
					if c.Err() != nil {
						return c.Err()
					}
				}
				b.BindWeek("2026-10-26", "week")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
				text := domain.NewCall(b, "week", nil, nil, clock.now()).WeekOverview()
				if !strings.Contains(text, "2026-10-26") || !strings.Contains(text, "2026-11-01") || strings.Contains(text, "2026-11-02") {
					t.Fatal(text)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAutomaticWeekAndEmptyAcceptance(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-11-01T21:00:00Z")
			books, jobs, reopen := workStores(t, backend, clock.now)
			appendScheduled(t, books, "2026-11-01", "one", "UTC", clock.now())
			k := &scheduleKernel{books: books, now: clock.now, progress: true, file: true, workKernel: workKernel{terminal: "completed"}}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			due := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC).Add(schedule.Jitter("bl", "u", "2026-10-26"))
			clock.at = due.Add(-time.Second)
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			for _, input := range k.inputs {
				if input.text == "回顾本周" {
					t.Fatal("week before due")
				}
			}
			clock.at = due
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			job := scheduledJob(t, jobs, "week", "2026-10-26", 1)
			if job.State != task.Succeeded {
				t.Fatal(job)
			}
			finish(t, o)
			books, jobs = reopen()
			k.books = books
			o = New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			clock.at = due.Add(24 * time.Hour)
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if err := o.ScanTasks(ctx); err != nil {
				t.Fatal(err)
			}
			weeks := 0
			for _, input := range k.inputs {
				if input.text == "回顾本周" {
					weeks++
				}
			}
			if weeks != 1 {
				t.Fatal("week duplicated after midnight/restart", weeks)
			}
			if pending, err := books.PendingDomainChanges(ctx, "bl", "u"); err != nil || len(pending) != 0 {
				t.Fatal("empty mapping not acknowledged", pending, err)
			}
		})
	}
}

func TestScheduleMetadataAndDuplicateBatch(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-28T09:00:00Z")
			books, jobs, _ := workStores(t, backend, clock.now)
			first := clock.now()
			appendScheduled(t, books, "2026-10-28", "one", "UTC", first)
			clock.at = first.Add(4 * time.Minute)
			appendScheduled(t, books, "2026-10-28", "one", "UTC", first)
			if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
				day := b.SchedulingDays()[0]
				if !day.LastBatchAt.Equal(first) || day.Revision != 1 || day.Processed {
					t.Fatal(day)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// 模拟 A9 的元数据形状；原文仍在同一事务的独立接缝中。
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				access := b.CaptureAccess()
				raw, err := json.Marshal(b)
				if err != nil {
					return err
				}
				var data map[string]json.RawMessage
				if err := json.Unmarshal(raw, &data); err != nil {
					return err
				}
				var days map[string]map[string]json.RawMessage
				if err := json.Unmarshal(data["CaptureDays"], &days); err != nil {
					return err
				}
				delete(days["2026-10-28"], "LastBatchAt")
				delete(days["2026-10-28"], "Tail")
				data["CaptureDays"], _ = json.Marshal(days)
				raw, _ = json.Marshal(data)
				if err := json.Unmarshal(raw, b); err != nil {
					return err
				}
				b.SetCaptureAccess(access)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			o := New(ctx, books, &deletionKernel{}, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
				day := b.SchedulingDays()[0]
				if !day.LastBatchAt.Equal(clock.now()) || day.Processed {
					t.Fatal("legacy metadata not initialized", day)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCaptureAppendDoesNotRetireInFlightSchedule(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-28T09:00:00Z")
			books, jobs, _ := workStores(t, backend, clock.now)
			appendScheduled(t, books, "2026-10-28", "one", "UTC", clock.now())
			k := &workKernel{}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			clock.at = clock.at.Add(5 * time.Minute)
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			job := scheduledJob(t, jobs, "day", "2026-10-28", 1)
			clock.at = job.RetryAt
			appendScheduled(t, books, "2026-10-28", "two", "UTC", clock.now())
			if err := o.ScanTasks(ctx); err != nil {
				t.Fatal(err)
			}
			after := scheduledJob(t, jobs, "day", "2026-10-28", 1)
			if after.State != task.Accepted || after.Session != job.Session || after.Retirements != 0 || len(k.inputs) != 1 {
				t.Fatal("append retired an active command", after)
			}
		})
	}
}

func TestAcceptedBusinessDoesNotWaitForRunTerminal(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-28T09:00:00Z")
			books, jobs, _ := workStores(t, backend, clock.now)
			appendScheduled(t, books, "2026-10-28", "one", "UTC", clock.now())
			k := &scheduleKernel{books: books, now: clock.now, progress: true, file: true}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			clock.at = clock.at.Add(5 * time.Minute)
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			job := scheduledJob(t, jobs, "day", "2026-10-28", 1)
			if job.State != task.Succeeded || job.Receipt.Terminal != "" || len(k.inputs) != 1 {
				t.Fatal("business completion depended on Run terminal", job)
			}
			if _, held, err := jobs.SessionTask(ctx, job.Owner, job.Session); err != nil || held {
				t.Fatal("completed business kept slot", held, err)
			}
			if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
				if !errors.Is(b.CheckWorkSession(job.Session), domain.ErrWorkRetired) {
					t.Fatal("released before retirement")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWeekUsesFirstBusinessDayTimeZone(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clock := &scheduleClock{}
			clock.set("2026-10-26T09:00:00Z")
			books, jobs, _ := workStores(t, backend, clock.now)
			appendScheduled(t, books, "2026-10-26", "one", "Pacific/Kiritimati", clock.now())
			clock.set("2026-11-01T09:00:00Z")
			appendScheduled(t, books, "2026-11-01", "one", "America/Los_Angeles", clock.now())
			k := &scheduleKernel{books: books, now: clock.now, progress: true, file: true, workKernel: workKernel{terminal: "completed"}}
			o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
			defer finish(t, o)
			z, err := time.LoadLocation("Pacific/Kiritimati")
			if err != nil {
				t.Fatal(err)
			}
			due := time.Date(2026, 11, 1, 23, 0, 0, 0, z).Add(schedule.Jitter("bl", "u", "2026-10-26"))
			clock.at = due.Add(-time.Second)
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			for _, input := range k.inputs {
				if input.text == "回顾本周" {
					t.Fatal("week ran before fixed time")
				}
			}
			clock.at = due
			if err := o.ScanSchedules(ctx); err != nil {
				t.Fatal(err)
			}
			if job := scheduledJob(t, jobs, "week", "2026-10-26", 1); job.State != task.Succeeded {
				t.Fatal("later time zone moved week", job)
			}
		})
	}
}
