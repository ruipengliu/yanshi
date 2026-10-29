// Package storetest 对内存和 PG 运行同一组租约、退避与 HTTP 对账契约。
package storetest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xinli/kernel"
	"xinli/task"
)

type Factory func(*testing.T) (task.Store, func() task.Store)

var epoch = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

func source(version uint64) task.Source {
	return task.Source{Owner: task.Owner{BusinessLine: "bl", EndUser: "u"}, Kind: "day", ID: "day/2026-10-28", Version: version}
}
func enqueue(t *testing.T, s task.Store, src task.Source, spec task.Spec) string {
	t.Helper()
	if err := s.Receive(context.Background(), src, []task.Spec{spec}, epoch); err != nil {
		t.Fatal(err)
	}
	return task.Identity(src, spec)
}
func get(t *testing.T, s task.Store, id string) task.Task {
	t.Helper()
	v, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func claim(t *testing.T, s task.Store, id string, now time.Time) task.Task {
	t.Helper()
	v, ok, err := s.Claim(context.Background(), id, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return v
}

func Run(t *testing.T, factory Factory) {
	ctx := context.Background()
	t.Run("receive_identity_and_frozen_mapping", func(t *testing.T) {
		s, _ := factory(t)
		spec := task.Spec{Operation: "collect", Target: "project/p", Date: "2026-10-28"}
		id := enqueue(t, s, source(1), spec)
		changed := spec
		changed.Target = "project/q"
		if err := s.Receive(ctx, source(1), []task.Spec{changed}, epoch); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, task.Identity(source(1), changed)); !errors.Is(err, task.ErrNotFound) {
			t.Fatal("reinterpreted receipt", err)
		}
		other := source(1)
		other.EndUser = "other"
		otherID := enqueue(t, s, other, spec)
		second := enqueue(t, s, source(2), spec)
		if id == otherID || id == second || get(t, s, id).InputID == get(t, s, second).InputID {
			t.Fatal("identity collision")
		}
		empty := source(3)
		if err := s.Receive(ctx, empty, nil, epoch); err != nil {
			t.Fatal(err)
		}
		if err := s.Receive(ctx, empty, []task.Spec{spec}, epoch); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, task.Identity(empty, spec)); !errors.Is(err, task.ErrNotFound) {
			t.Fatal("empty receipt lost", err)
		}
	})
	t.Run("two_claimants_and_stale_completion", func(t *testing.T) {
		s, _ := factory(t)
		id := enqueue(t, s, source(1), task.Spec{Operation: "collect", Target: "p"})
		var winners atomic.Int32
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, ok, err := s.Claim(ctx, id, epoch, time.Minute)
				if err != nil {
					t.Error(err)
				}
				if ok {
					winners.Add(1)
				}
			}()
		}
		wg.Wait()
		if winners.Load() != 1 {
			t.Fatal("multiple lease holders")
		}
		old := get(t, s, id)
		next := claim(t, s, id, epoch.Add(time.Minute))
		if next.LeaseGeneration != old.LeaseGeneration+1 || next.Attempt != 2 {
			t.Fatal(next)
		}
		old.State = task.Succeeded
		if err := s.Save(ctx, old, epoch.Add(time.Minute), true); !errors.Is(err, task.ErrLeaseLost) {
			t.Fatal("stale result accepted", err)
		}
		next.State = task.Succeeded
		if err := s.Save(ctx, next, epoch.Add(time.Minute), true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lease_expiry_and_backoff_survive_restart", func(t *testing.T) {
		s, reopen := factory(t)
		id := enqueue(t, s, source(1), task.Spec{Operation: "collect", Target: "p"})
		v := claim(t, s, id, epoch)
		v.RetryAt = epoch.Add(time.Hour)
		v.LastErrorClass = "retryable"
		if err := s.Save(ctx, v, epoch, true); err != nil {
			t.Fatal(err)
		}
		s = reopen()
		if _, ok, err := s.Claim(ctx, id, epoch.Add(time.Minute), time.Minute); err != nil || ok {
			t.Fatal("backoff lost", ok, err)
		}
		v = claim(t, s, id, epoch.Add(time.Hour))
		if v.Attempt != 2 {
			t.Fatal(v)
		}
		v = claim(t, s, id, epoch.Add(time.Hour+time.Minute))
		if v.LeaseGeneration != 3 {
			t.Fatal(v)
		}
	})
	t.Run("target_order_and_session_slot", func(t *testing.T) {
		s, _ := factory(t)
		spec := task.Spec{Operation: "collect", Target: "p"}
		first := enqueue(t, s, source(1), spec)
		second := enqueue(t, s, source(2), spec)
		if _, ok, err := s.Claim(ctx, second, epoch, time.Minute); err != nil || ok {
			t.Fatal("FIFO bypass", err)
		}
		a := claim(t, s, first, epoch)
		a.State = task.Unknown
		a.Session = "session"
		if err := s.Save(ctx, a, epoch, true); err != nil {
			t.Fatal(err)
		}
		third := enqueue(t, s, source(3), task.Spec{Operation: "rewrite", Target: "different-logical-target"})
		b := claim(t, s, third, epoch)
		b.State = task.Unknown
		b.Session = "session"
		if err := s.Save(ctx, b, epoch, false); !errors.Is(err, task.ErrTargetBusy) {
			t.Fatal("session slot bypass", err)
		}
		a = claim(t, s, first, epoch)
		a.State = task.Accepted
		a.Receipt = kernel.InputReceipt{InputID: a.InputID, RunID: "run"}
		if err := s.Save(ctx, a, epoch, true); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, b, epoch, false); !errors.Is(err, task.ErrTargetBusy) {
			t.Fatal("acceptance released slot", err)
		}
		a = claim(t, s, first, epoch)
		a.State = task.Succeeded
		a.Receipt.Terminal = "completed"
		if err := s.Save(ctx, a, epoch, true); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, b, epoch, false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("configuration_failure_count", func(t *testing.T) {
		s, _ := factory(t)
		id := enqueue(t, s, source(1), task.Spec{Operation: "unknown", Target: "p"})
		w := task.Executor{Store: s, Now: func() time.Time { return epoch }}
		if _, err := w.Run(ctx, id); !errors.Is(err, task.ErrConfiguration) {
			t.Fatal(err)
		}
		if v := get(t, s, id); v.State != task.Failed || v.LastErrorClass != "configuration" {
			t.Fatal(v)
		}
		if n, err := s.FailureCount(ctx, source(1).Owner); err != nil || n != 1 {
			t.Fatal(n, err)
		}
		if _, ok, err := s.Claim(ctx, id, epoch.Add(time.Hour), time.Minute); err != nil || ok {
			t.Fatal("configuration retried", err)
		}
	})
	t.Run("concurrent_session_slot", func(t *testing.T) {
		s, _ := factory(t)
		first := enqueue(t, s, source(1), task.Spec{Operation: "collect", Target: "p"})
		second := enqueue(t, s, source(2), task.Spec{Operation: "rewrite", Target: "q"})
		values := []task.Task{claim(t, s, first, epoch), claim(t, s, second, epoch)}
		var winners, busy atomic.Int32
		var wg sync.WaitGroup
		for _, value := range values {
			wg.Add(1)
			go func(v task.Task) {
				defer wg.Done()
				v.Session, v.State = "session", task.Unknown
				err := s.Save(ctx, v, epoch, false)
				if err == nil {
					winners.Add(1)
				} else if errors.Is(err, task.ErrTargetBusy) {
					busy.Add(1)
				} else {
					t.Error(err)
				}
			}(value)
		}
		wg.Wait()
		if winners.Load() != 1 || busy.Load() != 1 {
			t.Fatal("session slot has multiple owners")
		}
	})
	t.Run("stable_capped_backoff", func(t *testing.T) {
		base, ceiling := time.Second, 8*time.Second
		first := task.Backoff("task-id", 1, base, ceiling)
		second := task.Backoff("task-id", 2, base, ceiling)
		if first < 750*time.Millisecond || first > base || second != 2*first {
			t.Fatal(first, second)
		}
		last := task.Backoff("task-id", 1000, base, ceiling)
		if last < 6*time.Second || last > ceiling || last != task.Backoff("task-id", 1000, base, ceiling) {
			t.Fatal(last)
		}
	})
	t.Run("worker_http", func(t *testing.T) { workerTests(t, factory) })
}
