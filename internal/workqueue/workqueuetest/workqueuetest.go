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
	advance := func(t *testing.T, c *clock.Fake, q workqueue.Queue, d time.Duration) {
		c.Advance(d)
		if def, ok := q.(workqueue.Deferrer); ok {
			if _, err := def.Promote(ctx, c.Now().Add(def.Horizon()), 1000); err != nil {
				t.Fatal(err)
			}
		}
	}
	setup := func(t *testing.T) (*clock.Fake, workqueue.Queue) {
		c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		return c, newQueue(t, c)
	}

	t.Run("ClaimIsExclusiveUntilExpiry", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		l, err := q.Claim(ctx, "w1", time.Second, anyWork)
		if err != nil || l.SessionID != "ses_v1_000_000_s" {
			t.Fatalf("claim = %+v, %v", l, err)
		}
		if _, err := q.Claim(ctx, "w2", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("second claim err = %v", err)
		}
		advance(t, c, q, time.Second)
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
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		l, _ := q.Claim(ctx, "w1", 2*time.Second, anyWork)
		advance(t, c, q, time.Second)
		if err := q.Renew(ctx, l, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		advance(t, c, q, time.Second+500*time.Millisecond)
		if _, err := q.Claim(ctx, "w2", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("renewed lease was claimable: %v", err)
		}
	})

	t.Run("EnqueueIsIdempotent", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("duplicate item: %v", err)
		}
		_ = q.Release(ctx, l, true)
	})

	t.Run("RemoveEvenWhileLeased", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		_ = q.Enqueue(ctx, "ses_v1_000_000_t", interactive)
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
		if err := q.Remove(ctx, "ses_v1_000_000_missing"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ReleaseDoneRemoves", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
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
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		if err := q.Release(ctx, l, true); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); err != nil {
			t.Fatalf("re-enqueued session lost: %v", err)
		}
	})

	t.Run("ReleaseNotDoneRotates", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_a", interactive)
		_ = q.Enqueue(ctx, "ses_v1_000_000_b", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		if l.SessionID != "ses_v1_000_000_a" {
			t.Fatalf("claimed %s first, want a", l.SessionID)
		}
		_ = q.Release(ctx, l, false)
		l, _ = q.Claim(ctx, "w", time.Second, anyWork)
		if l.SessionID != "ses_v1_000_000_b" {
			t.Fatalf("claimed %s, want b", l.SessionID)
		}
	})

	t.Run("ParkHidesUntilDeadlineOrWake", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		if err := q.Park(ctx, l, c.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("parked session claimable: %v", err)
		}
		advance(t, c, q, time.Minute)
		l, err := q.Claim(ctx, "w", time.Second, anyWork)
		if err != nil {
			t.Fatalf("not claimable after deadline: %v", err)
		}
		_ = q.Park(ctx, l, c.Now().Add(time.Hour))
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); err != nil {
			t.Fatalf("not claimable after wake: %v", err)
		}
	})

	t.Run("ParkAfterEnqueueDuringLeaseStaysAvailable", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		l, _ := q.Claim(ctx, "w", time.Second, anyWork)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		_ = q.Park(ctx, l, c.Now().Add(time.Hour))
		if _, err := q.Claim(ctx, "w", time.Second, anyWork); err != nil {
			t.Fatalf("wake during lease lost: %v", err)
		}
	})

	t.Run("ClaimPrefersHigherPriority", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_long", background)
		_ = q.Enqueue(ctx, "ses_v1_000_000_a", interactive)
		_ = q.Enqueue(ctx, "ses_v1_000_000_b", interactive)
		var got []string
		for range 3 {
			l, err := q.Claim(ctx, "w", time.Minute, anyWork)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, l.SessionID)
		}
		if got[0] != "ses_v1_000_000_a" || got[1] != "ses_v1_000_000_b" || got[2] != "ses_v1_000_000_long" {
			t.Fatalf("claim order %v, want interactive work first, in enqueue order", got)
		}
	})

	t.Run("ClaimOnlyFromPool", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_long", background)
		_ = q.Enqueue(ctx, "ses_v1_000_000_other", workqueue.Class{BusinessLine: "other", Priority: workqueue.Interactive})
		if _, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{BusinessLines: []string{"bl"}, MinPriority: workqueue.Interactive}); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("claimed outside the pool: %v", err)
		}
		l, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{BusinessLines: []string{"other"}})
		if err != nil || l.SessionID != "ses_v1_000_000_other" || l.Class.BusinessLine != "other" || l.Class.Priority != workqueue.Interactive {
			t.Fatalf("claim by business line = %+v, %v", l, err)
		}
		l, err = q.Claim(ctx, "w", time.Minute, workqueue.Pool{BusinessLines: []string{"x", "bl"}})
		if err != nil || l.SessionID != "ses_v1_000_000_long" || l.Class != background {
			t.Fatalf("claim = %+v, %v", l, err)
		}
	})

	t.Run("EnqueueUpdatesClass", func(t *testing.T) {
		_, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", background)
		interactiveOnly := workqueue.Pool{MinPriority: workqueue.Interactive}
		if _, err := q.Claim(ctx, "w", time.Minute, interactiveOnly); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("class not updated by the second enqueue: %v", err)
		}
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		if l, err := q.Claim(ctx, "w", time.Minute, interactiveOnly); err != nil || l.Class != interactive {
			t.Fatalf("claim = %+v, %v", l, err)
		}
	})

	t.Run("HolderReclassifiesOnRelease", func(t *testing.T) {
		c, q := setup(t)
		interactiveOnly := workqueue.Pool{MinPriority: workqueue.Interactive}
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
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

	// 租约期间有人以 Resumed 入队（用户刚做出审批），持有者按尚未读到审批的投影以 Background 归还：恢复名额保留
	// （workqueue.ReturnedClass，评审 §11 问题 7）。没有这次入队时，持有者给的类别照常生效。
	t.Run("ReturnKeepsResumedEnqueuedDuringLease", func(t *testing.T) {
		resumed := workqueue.Class{BusinessLine: "bl", Priority: workqueue.Resumed}
		resumedOnly := workqueue.Pool{MinPriority: workqueue.Resumed}
		for _, park := range []bool{false, true} {
			c, q := setup(t)
			_ = q.Enqueue(ctx, "ses_v1_000_000_s", background)
			l, _ := q.Claim(ctx, "w", time.Minute, anyWork)
			_ = q.Enqueue(ctx, "ses_v1_000_000_s", resumed)
			l.Class = background
			var err error
			if park {
				err = q.Park(ctx, l, c.Now().Add(time.Hour))
			} else {
				err = q.Release(ctx, l, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			l, err = q.Claim(ctx, "w", time.Minute, resumedOnly)
			if err != nil || l.Class != resumed {
				t.Fatalf("park=%v: the resumed slot was lost: %+v, %v", park, l, err)
			}
			l.Class = background
			if err := q.Release(ctx, l, false); err != nil {
				t.Fatal(err)
			}
			if _, err := q.Claim(ctx, "w", time.Minute, resumedOnly); !errors.Is(err, workqueue.ErrEmpty) {
				t.Fatalf("park=%v: without an enqueue during the lease the holder's class did not apply: %v", park, err)
			}
		}
	})

	// Waited 是认领前已可认领的时长：入队、挂起到期、唤醒、归还、租约过期分别从各自的时刻算起（排队等待的指标）。
	t.Run("ClaimReportsWait", func(t *testing.T) {
		c, q := setup(t)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
		advance(t, c, q, 3*time.Second)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive) // 已在等的项再次入队不重新计时
		advance(t, c, q, time.Second)
		l, err := q.Claim(ctx, "w", time.Minute, anyWork)
		if err != nil || l.Waited != 4*time.Second {
			t.Fatalf("after enqueue: %+v, %v", l, err)
		}
		if err := q.Park(ctx, l, c.Now().Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		advance(t, c, q, 12*time.Second)
		if l, err = q.Claim(ctx, "w", time.Minute, anyWork); err != nil || l.Waited != 2*time.Second {
			t.Fatalf("after park deadline: %+v, %v", l, err)
		}
		if err := q.Park(ctx, l, c.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		advance(t, c, q, time.Minute)
		_ = q.Enqueue(ctx, "ses_v1_000_000_s", interactive) // 唤醒：从现在算起
		advance(t, c, q, time.Second)
		if l, err = q.Claim(ctx, "w", time.Second, anyWork); err != nil || l.Waited != time.Second {
			t.Fatalf("after wake: %+v, %v", l, err)
		}
		advance(t, c, q, 5*time.Second) // 租约 1 秒前过期
		if l, err = q.Claim(ctx, "w2", time.Minute, anyWork); err != nil || l.Waited != 4*time.Second {
			t.Fatalf("after lease expiry: %+v, %v", l, err)
		}
		if err := q.Release(ctx, l, false); err != nil {
			t.Fatal(err)
		}
		advance(t, c, q, 2*time.Second)
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
		_ = q.Enqueue(ctx, "ses_v1_000_000_a", interactive)
		advance(t, c, q, 5*time.Second)
		_ = q.Enqueue(ctx, "ses_v1_000_000_b", interactive)
		_ = q.Enqueue(ctx, "ses_v1_000_000_c", background)
		_ = q.Enqueue(ctx, "ses_v1_000_000_d", interactive)
		l, _ := q.Claim(ctx, "w", time.Minute, anyWork) // a：已租出，不算
		_ = q.Enqueue(ctx, "ses_v1_000_000_e", background)
		advance(t, c, q, time.Second)
		if l.SessionID != "ses_v1_000_000_a" {
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
	t.Run("FarParkLifecycleAndOrder", func(t *testing.T) {
		c, q := setup(t)
		d, ok := q.(workqueue.Deferrer)
		if !ok {
			t.Skip("no deferrer")
		}
		due := c.Now().Add(24 * time.Hour)
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		claim := func() *workqueue.Lease {
			t.Helper()
			l, err := q.Claim(ctx, "w", time.Minute, anyWork)
			must(err)
			return l
		}
		empty := func() {
			t.Helper()
			if _, err := q.Claim(ctx, "w", time.Minute, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
				t.Fatalf("claim = %v", err)
			}
		}
		for _, id := range []string{"ses_v1_000_000_removed", "ses_v1_000_000_wake", "ses_v1_000_000_again", "ses_v1_000_000_a", "ses_v1_000_000_b", "ses_v1_000_000_low"} {
			must(q.Enqueue(ctx, id, interactive))
			l := claim()
			if id == "ses_v1_000_000_low" {
				l.Class = background
			}
			must(q.Park(ctx, l, due))
		}
		empty()
		must(q.Remove(ctx, "ses_v1_000_000_removed"))
		must(q.Enqueue(ctx, "ses_v1_000_000_wake", interactive))
		l := claim()
		if l.SessionID != "ses_v1_000_000_wake" {
			t.Fatalf("wake = %s", l.SessionID)
		}
		must(q.Release(ctx, l, true))
		must(q.Enqueue(ctx, "ses_v1_000_000_again", interactive))
		l = claim()
		must(q.Park(ctx, l, due.Add(time.Hour)))
		_, err := d.Promote(ctx, due, 100)
		must(err)
		empty() // 搬回工作表仍须等到精确时刻
		c.Advance(24 * time.Hour)
		for _, id := range []string{"ses_v1_000_000_a", "ses_v1_000_000_b", "ses_v1_000_000_low"} {
			l = claim()
			if l.SessionID != id {
				t.Fatalf("order = %s, want %s", l.SessionID, id)
			}
			want := interactive
			if id == "ses_v1_000_000_low" {
				want = background
			}
			if l.Class != want {
				t.Fatalf("class = %+v", l.Class)
			}
			must(q.Release(ctx, l, true))
		}
		empty()
		_, err = d.Promote(ctx, due.Add(time.Hour), 100)
		must(err)
		must(q.Remove(ctx, "ses_v1_000_000_again")) // 搬移之后删除也不复活
		c.Advance(time.Hour)
		_, err = d.Promote(ctx, c.Now(), 100)
		must(err)
		empty()
	})

	t.Run("FarParkConcurrentMaintenance", func(t *testing.T) {
		for _, action := range []string{"wake", "remove", "repark"} {
			t.Run(action, func(t *testing.T) {
				c, q := setup(t)
				d, ok := q.(workqueue.Deferrer)
				if !ok {
					t.Skip("no deferrer")
				}
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				must(q.Enqueue(ctx, "ses_v1_000_000_s", interactive))
				l, err := q.Claim(ctx, "w", time.Minute, anyWork)
				must(err)
				until := c.Now().Add(24 * time.Hour)
				must(q.Park(ctx, l, until))
				start := make(chan struct{})
				results := make(chan error, 2)
				go func() {
					<-start
					_, err := d.Promote(ctx, until, 100)
					results <- err
				}()
				go func() {
					<-start
					if action == "remove" {
						results <- q.Remove(ctx, "ses_v1_000_000_s")
						return
					}
					err := q.Enqueue(ctx, "ses_v1_000_000_s", interactive)
					if err == nil && action == "repark" {
						var l *workqueue.Lease
						l, err = q.Claim(ctx, "w", time.Minute, anyWork)
						if err == nil {
							err = q.Park(ctx, l, until.Add(time.Hour))
						}
					}
					results <- err
				}()
				close(start)
				must(<-results)
				must(<-results)
				if action == "wake" {
					l, err := q.Claim(ctx, "w", time.Minute, anyWork)
					must(err)
					must(q.Release(ctx, l, true))
				}
				c.Advance(24 * time.Hour)
				_, err = d.Promote(ctx, c.Now(), 100)
				must(err)
				if _, err := q.Claim(ctx, "w", time.Minute, anyWork); !errors.Is(err, workqueue.ErrEmpty) {
					t.Fatalf("resurrected or early: %v", err)
				}
				if action == "repark" {
					c.Advance(time.Hour)
					_, err = d.Promote(ctx, c.Now(), 100)
					must(err)
					_, err = q.Claim(ctx, "w", time.Minute, anyWork)
					must(err)
				}
			})
		}
	})
}
