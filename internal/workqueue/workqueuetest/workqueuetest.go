// Package workqueuetest 是 workqueue.Queue 实现的一致性测试套件。
package workqueuetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/workqueue"
)

// Run 对 newQueue 返回的实现运行全部用例；每个用例获得全新的队列与虚拟时钟。
func Run(t *testing.T, newQueue func(t *testing.T, c clock.Clock) workqueue.Queue) {
	ctx := context.Background()
	setup := func(t *testing.T) (*clock.Fake, workqueue.Queue) {
		c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		return c, newQueue(t, c)
	}

	t.Run("ClaimIsExclusiveUntilExpiry", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s")
		l, err := q.Claim(ctx, "w1", time.Second)
		if err != nil || l.SessionID != "s" {
			t.Fatalf("claim = %+v, %v", l, err)
		}
		if _, err := q.Claim(ctx, "w2", time.Second); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("second claim err = %v", err)
		}
		c.Advance(time.Second)
		l2, err := q.Claim(ctx, "w2", time.Second)
		if err != nil || l2.Holder != "w2" {
			t.Fatalf("claim after expiry = %+v, %v", l2, err)
		}
		if err := q.Renew(ctx, l, time.Second); !errors.Is(err, workqueue.ErrLeaseLost) {
			t.Fatalf("stale renew err = %v", err)
		}
		if err := q.Release(ctx, l, true); !errors.Is(err, workqueue.ErrLeaseLost) {
			t.Fatalf("stale release err = %v", err)
		}
		if err := q.Park(ctx, l, c.Now()); !errors.Is(err, workqueue.ErrLeaseLost) {
			t.Fatalf("stale park err = %v", err)
		}
	})

	t.Run("RenewExtends", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s")
		l, _ := q.Claim(ctx, "w1", 2*time.Second)
		c.Advance(time.Second)
		if err := q.Renew(ctx, l, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Second + 500*time.Millisecond)
		if _, err := q.Claim(ctx, "w2", time.Second); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("renewed lease was claimable: %v", err)
		}
	})

	t.Run("EnqueueIsIdempotent", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s")
		_ = q.Enqueue(ctx, "s")
		l, _ := q.Claim(ctx, "w", time.Second)
		if _, err := q.Claim(ctx, "w", time.Second); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("duplicate item: %v", err)
		}
		_ = q.Release(ctx, l, true)
	})

	t.Run("ReleaseDoneRemoves", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s")
		l, _ := q.Claim(ctx, "w", time.Second)
		if err := q.Release(ctx, l, true); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("claim after done err = %v", err)
		}
	})

	t.Run("ReleaseDoneKeepsItemEnqueuedDuringLease", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s")
		l, _ := q.Claim(ctx, "w", time.Second)
		_ = q.Enqueue(ctx, "s")
		if err := q.Release(ctx, l, true); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second); err != nil {
			t.Fatalf("re-enqueued session lost: %v", err)
		}
	})

	t.Run("ReleaseNotDoneRotates", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "a")
		_ = q.Enqueue(ctx, "b")
		l, _ := q.Claim(ctx, "w", time.Second)
		if l.SessionID != "a" {
			t.Fatalf("claimed %s first, want a", l.SessionID)
		}
		_ = q.Release(ctx, l, false)
		l, _ = q.Claim(ctx, "w", time.Second)
		if l.SessionID != "b" {
			t.Fatalf("claimed %s, want b", l.SessionID)
		}
	})

	t.Run("ParkHidesUntilDeadlineOrWake", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s")
		l, _ := q.Claim(ctx, "w", time.Second)
		if err := q.Park(ctx, l, c.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("parked session claimable: %v", err)
		}
		c.Advance(time.Minute)
		l, err := q.Claim(ctx, "w", time.Second)
		if err != nil {
			t.Fatalf("not claimable after deadline: %v", err)
		}
		_ = q.Park(ctx, l, c.Now().Add(time.Hour))
		_ = q.Enqueue(ctx, "s")
		if _, err := q.Claim(ctx, "w", time.Second); err != nil {
			t.Fatalf("not claimable after wake: %v", err)
		}
	})

	t.Run("ParkAfterEnqueueDuringLeaseStaysAvailable", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s")
		l, _ := q.Claim(ctx, "w", time.Second)
		_ = q.Enqueue(ctx, "s")
		_ = q.Park(ctx, l, c.Now().Add(time.Hour))
		if _, err := q.Claim(ctx, "w", time.Second); err != nil {
			t.Fatalf("wake during lease lost: %v", err)
		}
	})
}
