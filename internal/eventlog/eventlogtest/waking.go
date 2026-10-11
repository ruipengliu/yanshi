package eventlogtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/workqueue"
)

// WakingStores 是同一存储上的日志、Session 工作队列与二者的原子组合。
type WakingStores struct {
	Log    eventlog.Log
	Queue  workqueue.Queue
	Waking eventlog.WakingLog
}

// RunWaking 是 eventlog.WakingLog 的组合套件（延展性评审 §8.2）：追加与入队同时生效或同时不生效，
// 并与认领、挂起、释放、删除正确交错。每个用例获得全新的存储与虚拟时钟。
func RunWaking(t *testing.T, newStores func(t *testing.T, c clock.Clock) WakingStores) {
	ctx := context.Background()
	class := workqueue.Class{BusinessLine: "bl", Priority: workqueue.Interactive}
	setup := func(t *testing.T) (*clock.Fake, WakingStores) {
		c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		return c, newStores(t, c)
	}
	claim := func(t *testing.T, q workqueue.Queue) *workqueue.Lease {
		t.Helper()
		l, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return l
	}
	empty := func(t *testing.T, q workqueue.Queue) {
		t.Helper()
		if l, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{}); !errors.Is(err, workqueue.ErrEmpty) {
			t.Fatalf("claim = %+v, %v; want empty", l, err)
		}
	}

	t.Run("AppendsAndEnqueues", func(t *testing.T) {
		_, s := setup(t)
		head, err := s.Waking.AppendAndEnqueue(ctx, "s", 0, class, ev("a"), ev("b"))
		if err != nil || head != 2 {
			t.Fatalf("append = %d, %v", head, err)
		}
		got, err := eventlog.ReadAll(ctx, s.Log, "s", 0)
		if err != nil || len(got) != 2 || got[1].GetSeq() != 2 {
			t.Fatalf("read = %v, %v", ids(got), err)
		}
		if l := claim(t, s.Queue); l.SessionID != "s" || l.Class != class {
			t.Fatalf("lease = %+v", l)
		}
	})

	t.Run("ConflictLeavesQueueUnchanged", func(t *testing.T) {
		_, s := setup(t)
		if _, err := s.Log.Append(ctx, "s", 0, ev("a")); err != nil {
			t.Fatal(err)
		}
		for _, expected := range []uint64{0, 2} {
			if _, err := s.Waking.AppendAndEnqueue(ctx, "s", expected, class, ev("x")); !errors.Is(err, eventlog.ErrConflict) {
				t.Fatalf("expected %d: err = %v", expected, err)
			}
		}
		empty(t, s.Queue)
		// 以同一批事件重试（"提交成功但回包丢失"后的重试）：第一次生效，第二次冲突，队列中只有一项。
		e := ev("b")
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 1, class, e); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 1, class, ev("b")); !errors.Is(err, eventlog.ErrConflict) {
			t.Fatalf("retry err = %v", err)
		}
		l := claim(t, s.Queue)
		empty(t, s.Queue)
		if err := s.Queue.Release(ctx, l, true); err != nil {
			t.Fatal(err)
		}
		empty(t, s.Queue)
	})

	t.Run("WakesParked", func(t *testing.T) {
		c, s := setup(t)
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 0, class, ev("a")); err != nil {
			t.Fatal(err)
		}
		l := claim(t, s.Queue)
		if err := s.Queue.Park(ctx, l, c.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		empty(t, s.Queue)
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 1, class, ev("b")); err != nil {
			t.Fatal(err)
		}
		claim(t, s.Queue)
	})

	t.Run("DuringLeaseSurvivesParkAndRelease", func(t *testing.T) {
		c, s := setup(t)
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 0, class, ev("a")); err != nil {
			t.Fatal(err)
		}
		// 持有者读到的日志不含随后的追加，据此挂起或以 done 释放：追加仍须被看到。
		l := claim(t, s.Queue)
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 1, class, ev("b")); err != nil {
			t.Fatal(err)
		}
		if err := s.Queue.Park(ctx, l, c.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		l = claim(t, s.Queue)
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 2, class, ev("c")); err != nil {
			t.Fatal(err)
		}
		if err := s.Queue.Release(ctx, l, true); err != nil {
			t.Fatal(err)
		}
		claim(t, s.Queue)
	})

	t.Run("NoResurrectionAfterDelete", func(t *testing.T) {
		_, s := setup(t)
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 0, class, ev("a")); err != nil {
			t.Fatal(err)
		}
		// 删除（ADR-0015）：清除日志与队列项之后，基于旧投影的迟到写入冲突，不留下队列项。
		if err := s.Queue.Remove(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		if err := s.Log.Delete(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 1, class, ev("b")); !errors.Is(err, eventlog.ErrConflict) {
			t.Fatalf("late append err = %v", err)
		}
		empty(t, s.Queue)
	})

	t.Run("InvalidEventsLeaveQueueUnchanged", func(t *testing.T) {
		_, s := setup(t)
		bad := ev("a")
		bad.GetSteered().RunId = "\xff" // 非法 UTF-8 无法序列化
		if _, err := s.Waking.AppendAndEnqueue(ctx, "s", 0, class, bad); err == nil {
			t.Fatal("append of unserializable event succeeded")
		}
		empty(t, s.Queue)
	})
}
