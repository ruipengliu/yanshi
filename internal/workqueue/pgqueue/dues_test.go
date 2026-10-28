package pgqueue

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/workqueue"
)

// 固定候选扫描和逐键搬移之间的窗口，不依靠 goroutine 调度碰巧命中。
func TestStaleDueCandidate(t *testing.T) {
	for _, action := range []string{"enqueue", "remove", "park", "promote"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			pool, _ := pgtest.Fresh(t)
			c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			q := New(pool, c, Sessions).WithHorizon(time.Second)
			class := workqueue.Class{BusinessLine: "bl", Priority: workqueue.Interactive}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			claim := func() *workqueue.Lease {
				t.Helper()
				l, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{})
				must(err)
				return l
			}
			must(q.Enqueue(ctx, "s", class))
			until := c.Now().Add(time.Hour)
			must(q.Park(ctx, claim(), until))
			d := due{sid: "s"}
			must(pool.QueryRow(ctx, `SELECT ord FROM work_dues WHERE session_id = $1`, d.sid).Scan(&d.ord))
			switch action {
			case "enqueue":
				must(q.Enqueue(ctx, "s", class))
			case "remove":
				must(q.Remove(ctx, "s"))
			case "park":
				must(q.Enqueue(ctx, "s", class))
				// 新期限仍在本轮 before 内，必须靠 ord 识别旧候选。
				must(q.Park(ctx, claim(), until.Add(-time.Minute)))
			case "promote":
				n, err := q.promote(ctx, d, until)
				must(err)
				if n != 1 {
					t.Fatalf("first promote = %d", n)
				}
			}
			n, err := q.promote(ctx, d, until)
			must(err)
			if n != 0 {
				t.Fatalf("stale candidate moved %d", n)
			}
			if action == "enqueue" {
				must(q.Release(ctx, claim(), true))
			} else if _, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{}); !errors.Is(err, workqueue.ErrEmpty) {
				t.Fatalf("unexpected claim: %v", err)
			}
			if action == "remove" {
				has, err := q.Contains(ctx, "s")
				must(err)
				if has {
					t.Fatal("removed session resurrected")
				}
			}
		})
	}
}
