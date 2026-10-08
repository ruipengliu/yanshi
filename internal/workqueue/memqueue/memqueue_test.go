package memqueue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

var ctx = context.Background()

func TestClaimIsExclusiveUntilExpiry(t *testing.T) {
	c := clock.NewFake(time.Unix(0, 0))
	q := memqueue.New(c)
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
}

func TestReleaseDoneKeepsItemEnqueuedDuringLease(t *testing.T) {
	q := memqueue.New(clock.NewFake(time.Unix(0, 0)))
	_ = q.Enqueue(ctx, "s")
	l, _ := q.Claim(ctx, "w", time.Second)
	_ = q.Enqueue(ctx, "s")
	if err := q.Release(ctx, l, true); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(ctx, "w", time.Second); err != nil {
		t.Fatalf("re-enqueued session lost: %v", err)
	}
}

func TestReleaseDoneRemoves(t *testing.T) {
	q := memqueue.New(clock.NewFake(time.Unix(0, 0)))
	_ = q.Enqueue(ctx, "s")
	l, _ := q.Claim(ctx, "w", time.Second)
	if err := q.Release(ctx, l, true); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(ctx, "w", time.Second); !errors.Is(err, workqueue.ErrEmpty) {
		t.Fatalf("claim after done err = %v", err)
	}
}

func TestReleaseNotDoneRotates(t *testing.T) {
	q := memqueue.New(clock.NewFake(time.Unix(0, 0)))
	_ = q.Enqueue(ctx, "a")
	_ = q.Enqueue(ctx, "b")
	l, _ := q.Claim(ctx, "w", time.Second)
	_ = q.Release(ctx, l, false)
	l, _ = q.Claim(ctx, "w", time.Second)
	if l.SessionID != "b" {
		t.Fatalf("claimed %s, want b", l.SessionID)
	}
}

func TestParkHidesUntilDeadlineOrWake(t *testing.T) {
	c := clock.NewFake(time.Unix(0, 0))
	q := memqueue.New(c)
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
}

func TestParkAfterEnqueueDuringLeaseStaysAvailable(t *testing.T) {
	c := clock.NewFake(time.Unix(0, 0))
	q := memqueue.New(c)
	_ = q.Enqueue(ctx, "s")
	l, _ := q.Claim(ctx, "w", time.Second)
	_ = q.Enqueue(ctx, "s")
	_ = q.Park(ctx, l, c.Now().Add(time.Hour))
	if _, err := q.Claim(ctx, "w", time.Second); err != nil {
		t.Fatalf("wake during lease lost: %v", err)
	}
}
