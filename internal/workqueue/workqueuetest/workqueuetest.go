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

var (
	interactive = workqueue.Class{BusinessLine: "bl", Priority: workqueue.Interactive}
	background  = workqueue.Class{BusinessLine: "bl", Priority: workqueue.Background}
	anyWork     = workqueue.Pool{}
)

// Run 对 newQueue 返回的实现运行全部用例；每个用例获得全新的队列与虚拟时钟。用例中的顺序断言都在一个分片内：
// 分片的实现按键路由时，以同一分片的键运行本套件（契约是分片内有序，延展性评审 §8.2）。
func Run(t *testing.T, newQueue func(t *testing.T, c clock.Clock) workqueue.Queue) {
	ctx := context.Background()
	setup := func(t *testing.T) (*clock.Fake, workqueue.Queue) {
		c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		return c, newQueue(t, c)
	}

	t.Run("ClaimIsExclusiveUntilExpiry", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		l, err := q.Claim(ctx, "w1", time.Second, anyWork)
		if err != nil || l.SessionID != "s" {
			t.Fatalf("claim = %+v, %v", l, err)
		}
		if _, err := q.Claim(ctx, "w2", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("second claim err = %v", err)
		}
		c.Advance(time.Second)
		l2, err := q.Claim(ctx, "w2", time.Second, anyWork)
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
		_ = q.Enqueue(ctx, "s", interactive)
		l, _ := q.Claim(ctx, "w1", 2*time.Second, anyWork)
		c.Advance(time.Second)
		if err := q.Renew(ctx, l, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Second + 500*time.Millisecond)
		if _, err := q.Claim(ctx, "w2", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("renewed lease was claimable: %v", err)
		}
	})

	t.Run("EnqueueIsIdempotent", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		_ = q.Enqueue(ctx, "s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("duplicate item: %v", err)
		}
		_ = q.Release(ctx, l, true)
	})

	t.Run("RemoveEvenWhileLeased", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		_ = q.Enqueue(ctx, "t", interactive)
		l, _ := q.Claim(ctx, "w1", time.Minute, anyWork)
		if err := q.Remove(ctx, l.SessionID); err != nil {
			t.Fatal(err)
		}
		if err := q.Renew(ctx, l, time.Minute); !errors.Is(err, workqueue.ErrLeaseLost) {
			t.Fatalf("renew after remove = %v", err)
		}
		next, err := q.Claim(ctx, "w2", time.Minute, anyWork)
		if err != nil || next.SessionID == l.SessionID {
			t.Fatalf("claim = %+v, %v", next, err)
		}
		if _, err := q.Claim(ctx, "w3", time.Minute, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("removed item still claimable: %v", err)
		}
		if err := q.Remove(ctx, "missing"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ReleaseDoneRemoves", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		if err := q.Release(ctx, l, true); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("claim after done err = %v", err)
		}
	})

	t.Run("ReleaseDoneKeepsItemEnqueuedDuringLease", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		_ = q.Enqueue(ctx, "s", interactive)
		if err := q.Release(ctx, l, true); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); err != nil {
			t.Fatalf("re-enqueued session lost: %v", err)
		}
	})

	t.Run("ReleaseNotDoneRotates", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "a", interactive)
		_ = q.Enqueue(ctx, "b", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		if l.SessionID != "a" {
			t.Fatalf("claimed %s first, want a", l.SessionID)
		}
		_ = q.Release(ctx, l, false)
		l, _ = q.Claim(ctx, "w", time.Second, anyWork)
		if l.SessionID != "b" {
			t.Fatalf("claimed %s, want b", l.SessionID)
		}
	})

	t.Run("ParkHidesUntilDeadlineOrWake", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		if err := q.Park(ctx, l, c.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("parked session claimable: %v", err)
		}
		c.Advance(time.Minute)
		l, err := q.Claim(ctx, "w", time.Second, anyWork)
		if err != nil {
			t.Fatalf("not claimable after deadline: %v", err)
		}
		_ = q.Park(ctx, l, c.Now().Add(time.Hour))
		_ = q.Enqueue(ctx, "s", interactive)
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); err != nil {
			t.Fatalf("not claimable after wake: %v", err)
		}
	})

	t.Run("ParkAfterEnqueueDuringLeaseStaysAvailable", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		_ = q.Enqueue(ctx, "s", interactive)
		_ = q.Park(ctx, l, c.Now().Add(time.Hour))
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); err != nil {
			t.Fatalf("wake during lease lost: %v", err)
		}
	})

	t.Run("ClaimPrefersHigherPriority", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "long", background)
		_ = q.Enqueue(ctx, "a", interactive)
		_ = q.Enqueue(ctx, "b", interactive)
		var got []string
		for range 3 {
			l, err := q.Claim(ctx, "w", time.Minute, anyWork)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, l.SessionID)
		}
		if got[0] != "a" || got[1] != "b" || got[2] != "long" {
			t.Fatalf("claim order %v, want interactive work first, in enqueue order", got)
		}
	})

	t.Run("ClaimOnlyFromPool", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "long", background)
		_ = q.Enqueue(ctx, "other", workqueue.Class{BusinessLine: "other", Priority: workqueue.Interactive})
		if _, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{BusinessLines: []string{"bl"}, MinPriority: workqueue.Interactive}); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("claimed outside the pool: %v", err)
		}
		l, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{BusinessLines: []string{"other"}})
		if err != nil || l.SessionID != "other" || l.Class.BusinessLine != "other" || l.Class.Priority != workqueue.Interactive {
			t.Fatalf("claim by business line = %+v, %v", l, err)
		}
		l, err = q.Claim(ctx, "w", time.Minute, workqueue.Pool{BusinessLines: []string{"x", "bl"}})
		if err != nil || l.SessionID != "long" || l.Class != background {
			t.Fatalf("claim = %+v, %v", l, err)
		}
	})

	t.Run("EnqueueUpdatesClass", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		_ = q.Enqueue(ctx, "s", background)
		interactiveOnly := workqueue.Pool{MinPriority: workqueue.Interactive}
		if _, err := q.Claim(ctx, "w", time.Minute, interactiveOnly); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("class not updated by the second enqueue: %v", err)
		}
		_ = q.Enqueue(ctx, "s", interactive)
		if l, err := q.Claim(ctx, "w", time.Minute, interactiveOnly); err != nil || l.Class != interactive {
			t.Fatalf("claim = %+v, %v", l, err)
		}
	})

	t.Run("HolderReclassifiesOnRelease", func(t *testing.T) {
		c, q := setup(t)
		interactiveOnly := workqueue.Pool{MinPriority: workqueue.Interactive}
		_ = q.Enqueue(ctx, "s", interactive)
		l, _ := q.Claim(ctx, "w", time.Minute, anyWork)
		l.Class = background
		if err := q.Release(ctx, l, false); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Minute, interactiveOnly); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("release kept the old class: %v", err)
		}
		l, err := q.Claim(ctx, "w", time.Minute, anyWork)
		if err != nil || l.Class != background {
			t.Fatalf("claim = %+v, %v", l, err)
		}
		l.Class = interactive
		if err := q.Park(ctx, l, c.Now()); err != nil {
			t.Fatal(err)
		}
		if l, err := q.Claim(ctx, "w", time.Minute, interactiveOnly); err != nil || l.Class != interactive {
			t.Fatalf("park kept the old class: %+v, %v", l, err)
		}
	})

	// Waited 是认领前已可认领的时长：入队、挂起到期、唤醒、归还、租约过期分别从各自的时刻算起（排队等待的指标）。
	t.Run("ClaimReportsWait", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "s", interactive)
		c.Advance(3 * time.Second)
		_ = q.Enqueue(ctx, "s", interactive) // 已在等的项再次入队不重新计时
		c.Advance(time.Second)
		l, err := q.Claim(ctx, "w", time.Minute, anyWork)
		if err != nil || l.Waited != 4*time.Second {
			t.Fatalf("after enqueue: %+v, %v", l, err)
		}
		if err := q.Park(ctx, l, c.Now().Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		c.Advance(12 * time.Second)
		if l, err = q.Claim(ctx, "w", time.Minute, anyWork); err != nil || l.Waited != 2*time.Second {
			t.Fatalf("after park deadline: %+v, %v", l, err)
		}
		if err := q.Park(ctx, l, c.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Minute)
		_ = q.Enqueue(ctx, "s", interactive) // 唤醒：从现在算起
		c.Advance(time.Second)
		if l, err = q.Claim(ctx, "w", time.Second, anyWork); err != nil || l.Waited != time.Second {
			t.Fatalf("after wake: %+v, %v", l, err)
		}
		c.Advance(5 * time.Second) // 租约 1 秒前过期
		if l, err = q.Claim(ctx, "w2", time.Minute, anyWork); err != nil || l.Waited != 4*time.Second {
			t.Fatalf("after lease expiry: %+v, %v", l, err)
		}
		if err := q.Release(ctx, l, false); err != nil {
			t.Fatal(err)
		}
		c.Advance(2 * time.Second)
		if l, err = q.Claim(ctx, "w", time.Minute, anyWork); err != nil || l.Waited != 2*time.Second {
			t.Fatalf("after release: %+v, %v", l, err)
		}
	})

	t.Run("DepthsCountClaimableWork", func(t *testing.T) {
		c, q := setup(t)
		st, ok := q.(workqueue.Stats)
		if !ok {
			t.Skip("queue does not implement workqueue.Stats")
		}
		_ = q.Enqueue(ctx, "a", interactive)
		c.Advance(5 * time.Second)
		_ = q.Enqueue(ctx, "b", interactive)
		_ = q.Enqueue(ctx, "c", background)
		_ = q.Enqueue(ctx, "d", interactive)
		l, _ := q.Claim(ctx, "w", time.Minute, anyWork) // a：已租出，不算
		_ = q.Enqueue(ctx, "e", background)
		c.Advance(time.Second)
		if l.SessionID != "a" {
			t.Fatalf("claimed %s", l.SessionID)
		}
		parked, _ := q.Claim(ctx, "w", time.Minute, anyWork) // b：挂起中，不算
		if err := q.Park(ctx, parked, c.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		depths, err := st.Depths(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[workqueue.Class]workqueue.Depth{}
		for _, d := range depths {
			got[d.Class] = d
		}
		if d := got[interactive]; d.Ready != 1 || d.OldestWait != time.Second {
			t.Fatalf("interactive depth %+v (all %+v)", d, depths)
		}
		if d := got[background]; d.Ready != 2 || d.OldestWait != time.Second {
			t.Fatalf("background depth %+v (all %+v)", d, depths)
		}
	})
}
