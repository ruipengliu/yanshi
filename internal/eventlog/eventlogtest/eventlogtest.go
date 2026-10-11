// Package eventlogtest 是 eventlog.Log 实现的一致性测试套件。
// 每个实现都应在自己的测试中调用 Run。
package eventlogtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
)

func ev(id string) *v1.Event {
	return &v1.Event{Id: id, Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: "r"}}}
}

func ids(es []*v1.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.GetId()
	}
	return out
}

func Run(t *testing.T, newLog func(t *testing.T) eventlog.Log) {
	ctx := context.Background()

	t.Run("AppendAssignsContiguousSeq", func(t *testing.T) {
		l := newLog(t)
		head, err := l.Append(ctx, "s", 0, ev("a"), ev("b"))
		if err != nil || head != 2 {
			t.Fatalf("append = %d, %v", head, err)
		}
		head, err = l.Append(ctx, "s", 2, ev("c"))
		if err != nil || head != 3 {
			t.Fatalf("append = %d, %v", head, err)
		}
		got, err := eventlog.ReadAll(ctx, l, "s", 0)
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range got {
			if e.GetSeq() != uint64(i+1) || e.GetSessionId() != "s" {
				t.Fatalf("event %d has seq %d session %q", i, e.GetSeq(), e.GetSessionId())
			}
		}
		if g := ids(got); len(g) != 3 || g[0] != "a" || g[2] != "c" {
			t.Fatalf("read %v", g)
		}
	})

	t.Run("AppendConflictIsAtomic", func(t *testing.T) {
		l := newLog(t)
		if _, err := l.Append(ctx, "s", 0, ev("a")); err != nil {
			t.Fatal(err)
		}
		for _, expected := range []uint64{0, 2} {
			if _, err := l.Append(ctx, "s", expected, ev("x"), ev("y")); !errors.Is(err, eventlog.ErrConflict) {
				t.Fatalf("expected=%d: err = %v, want ErrConflict", expected, err)
			}
		}
		got, _ := eventlog.ReadAll(ctx, l, "s", 0)
		if len(got) != 1 {
			t.Fatalf("conflicting append leaked events: %v", ids(got))
		}
	})

	// 冲突的追加不改动调用方的事件：调用方同步后以同一批事件重试（session.Store 的冲突重试）。
	t.Run("ConflictLeavesEventsUntouched", func(t *testing.T) {
		l := newLog(t)
		if _, err := l.Append(ctx, "s", 0, ev("a")); err != nil {
			t.Fatal(err)
		}
		x := ev("x")
		if _, err := l.Append(ctx, "s", 0, x); !errors.Is(err, eventlog.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if x.GetSeq() != 0 || x.GetSessionId() != "" {
			t.Fatalf("failed append set seq %d session %q", x.GetSeq(), x.GetSessionId())
		}
		if _, err := l.Append(ctx, "s", 1, x); err != nil {
			t.Fatal(err)
		}
		if x.GetSeq() != 2 || x.GetSessionId() != "s" {
			t.Fatalf("append set seq %d session %q, want 2 s", x.GetSeq(), x.GetSessionId())
		}
	})

	t.Run("SessionsAreIndependent", func(t *testing.T) {
		l := newLog(t)
		if _, err := l.Append(ctx, "s1", 0, ev("a")); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Append(ctx, "s2", 0, ev("b")); err != nil {
			t.Fatal(err)
		}
		got, _ := eventlog.ReadAll(ctx, l, "s2", 0)
		if g := ids(got); len(g) != 1 || g[0] != "b" {
			t.Fatalf("s2 = %v", g)
		}
	})

	t.Run("ReadAfterAndLimit", func(t *testing.T) {
		l := newLog(t)
		if _, err := l.Append(ctx, "s", 0, ev("a"), ev("b"), ev("c"), ev("d")); err != nil {
			t.Fatal(err)
		}
		got, _ := l.Read(ctx, "s", 1, 2)
		if g := ids(got); len(g) != 2 || g[0] != "b" || g[1] != "c" {
			t.Fatalf("read = %v", g)
		}
		got, _ = l.Read(ctx, "s", 4, 0)
		if len(got) != 0 {
			t.Fatalf("read past head = %v", ids(got))
		}
		got, _ = l.Read(ctx, "missing", 0, 0)
		if len(got) != 0 {
			t.Fatalf("read missing session = %v", ids(got))
		}
	})

	t.Run("AppendedEventsAreNotAliased", func(t *testing.T) {
		l := newLog(t)
		e := ev("a")
		if _, err := l.Append(ctx, "s", 0, e); err != nil {
			t.Fatal(err)
		}
		e.Id = "mutated"
		got, _ := eventlog.ReadAll(ctx, l, "s", 0)
		if got[0].GetId() != "a" {
			t.Fatalf("stored event changed after caller mutation")
		}
	})

	t.Run("WaitWakesOnAppend", func(t *testing.T) {
		l := newLog(t)
		var wg sync.WaitGroup
		wg.Add(1)
		var head uint64
		var werr error
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			head, werr = l.Wait(c, "s", 0)
		}()
		time.Sleep(10 * time.Millisecond)
		if _, err := l.Append(ctx, "s", 0, ev("a")); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if werr != nil || head != 1 {
			t.Fatalf("wait = %d, %v", head, werr)
		}
	})

	t.Run("WaitReturnsImmediatelyWhenBehind", func(t *testing.T) {
		l := newLog(t)
		if _, err := l.Append(ctx, "s", 0, ev("a")); err != nil {
			t.Fatal(err)
		}
		head, err := l.Wait(ctx, "s", 0)
		if err != nil || head != 1 {
			t.Fatalf("wait = %d, %v", head, err)
		}
	})

	t.Run("RejectsUnserializableEvents", func(t *testing.T) {
		l := newLog(t)
		bad := &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: "r\xff"}}}
		if _, err := l.Append(ctx, "s", 0, ev("a"), bad); err == nil {
			t.Fatal("accepted an event with invalid UTF-8")
		}
		if got, _ := eventlog.ReadAll(ctx, l, "s", 0); len(got) != 0 {
			t.Fatalf("partial append: %d events", len(got))
		}
	})

	t.Run("DeleteRemovesOnlyThatSession", func(t *testing.T) {
		l := newLog(t)
		_, _ = l.Append(ctx, "s", 0, ev("a"), ev("b"))
		_, _ = l.Append(ctx, "other", 0, ev("x"))
		if err := l.Delete(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		if got, _ := eventlog.ReadAll(ctx, l, "s", 0); len(got) != 0 {
			t.Fatalf("deleted session still has %d events", len(got))
		}
		if got, _ := eventlog.ReadAll(ctx, l, "other", 0); len(got) != 1 {
			t.Fatal("delete touched another session")
		}
		if err := l.Delete(ctx, "s"); err != nil {
			t.Fatalf("delete is not idempotent: %v", err)
		}
	})

	t.Run("WaitHonorsContext", func(t *testing.T) {
		l := newLog(t)
		c, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if _, err := l.Wait(c, "s", 0); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait err = %v", err)
		}
	})
}
