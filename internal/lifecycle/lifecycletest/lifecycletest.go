// Package lifecycletest 是 lifecycle.Index 与 lifecycle.Deletions 实现的一致性测试套件。
package lifecycletest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func RunIndex(t *testing.T, newIndex func(t *testing.T) lifecycle.Index) {
	ctx := context.Background()
	put := func(t *testing.T, x lifecycle.Index, id, bl, eu string, at time.Time) {
		t.Helper()
		if err := x.Put(ctx, &lifecycle.Session{ID: id, BusinessLine: bl, EndUser: eu, Agent: "a", CreatedAt: at, LastInputAt: at}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("PutIsIdempotentAndTouchMonotonic", func(t *testing.T) {
		x := newIndex(t)
		put(t, x, "s1", "bl", "u1", t0)
		put(t, x, "s1", "bl", "other", t0.Add(time.Hour)) // 已存在：不变
		_ = x.Touch(ctx, "s1", t0.Add(2*time.Hour))
		_ = x.Touch(ctx, "s1", t0.Add(time.Hour)) // 回退：忽略
		s, err := x.Get(ctx, "s1")
		if err != nil || s.EndUser != "u1" || !s.LastInputAt.Equal(t0.Add(2*time.Hour)) || !s.CreatedAt.Equal(t0) || !s.ClosedAt.IsZero() {
			t.Fatalf("get = %+v, %v", s, err)
		}
		if _, err := x.Get(ctx, "nope"); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})

	t.Run("ListPagesNewestFirstWithinScope", func(t *testing.T) {
		x := newIndex(t)
		for i := range 5 {
			put(t, x, fmt.Sprintf("s%d", i), "bl", "u1", t0.Add(time.Duration(i)*time.Minute))
		}
		put(t, x, "same-a", "bl", "u1", t0.Add(10*time.Minute)) // 同一时间：按 ID 区分顺序
		put(t, x, "same-b", "bl", "u1", t0.Add(10*time.Minute))
		put(t, x, "x", "bl", "u2", t0.Add(time.Hour))
		put(t, x, "y", "other", "u1", t0.Add(time.Hour))

		var got []string
		cursor := ""
		for range 10 {
			page, next, err := x.List(ctx, "bl", "u1", cursor, 3)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range page {
				got = append(got, s.ID)
			}
			if next == "" {
				break
			}
			cursor = next
		}
		want := []string{"same-b", "same-a", "s4", "s3", "s2", "s1", "s0"}
		if !slices.Equal(got, want) {
			t.Fatalf("pages = %v, want %v", got, want)
		}
		all, _, _ := x.List(ctx, "bl", "", "", 100)
		if len(all) != 8 || all[0].ID != "x" {
			t.Fatalf("whole business line = %d, first %s", len(all), all[0].ID)
		}
		if _, _, err := x.List(ctx, "bl", "u1", "garbage", 3); err == nil {
			t.Fatal("accepted an invalid cursor")
		}
	})

	t.Run("RetentionByAgent", func(t *testing.T) { runRetentionByAgent(t, newIndex(t)) })

	t.Run("RetentionQueriesAndDelete", func(t *testing.T) {
		x := newIndex(t)
		put(t, x, "old", "bl", "u1", t0)
		put(t, x, "new", "bl", "u1", t0.Add(48*time.Hour))
		put(t, x, "closed", "bl", "u2", t0)
		put(t, x, "elsewhere", "other", "u1", t0)
		_ = x.MarkClosed(ctx, "closed", t0.Add(time.Hour))
		_ = x.MarkClosed(ctx, "closed", t0.Add(5*time.Hour)) // 已关闭：不变

		all := lifecycle.AgentFilter{}
		idle, _ := x.IdleBefore(ctx, "bl", all, t0.Add(24*time.Hour), 10)
		if !slices.Equal(idle, []string{"old"}) {
			t.Fatalf("idle = %v", idle)
		}
		if c, _ := x.ClosedBefore(ctx, "bl", all, t0.Add(2*time.Hour), 10); !slices.Equal(c, []string{"closed"}) {
			t.Fatalf("closed before = %v", c)
		}
		if c, _ := x.ClosedBefore(ctx, "bl", all, t0.Add(30*time.Minute), 10); len(c) != 0 {
			t.Fatalf("closed before (too early) = %v", c)
		}
		ids, _ := x.IDsOf(ctx, "bl", "u1")
		slices.Sort(ids)
		if !slices.Equal(ids, []string{"new", "old"}) {
			t.Fatalf("ids of u1 = %v", ids)
		}
		_ = x.Delete(ctx, "old")
		if _, err := x.Get(ctx, "old"); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatal("deleted session still indexed")
		}
	})
}

// RetentionByAgent：保留策略按 AgentDef 覆盖（docs/design/captures.md §8）。只含某个 AgentDef 与排除若干 AgentDef 的查询
// 互补，一个 Session 恰好落在一条规则中。
func runRetentionByAgent(t *testing.T, x lifecycle.Index) {
	ctx := context.Background()
	for _, s := range []*lifecycle.Session{
		{ID: "day1", Agent: "day"}, {ID: "day2", Agent: "day"}, {ID: "proj", Agent: "project"}, {ID: "chat", Agent: "assistant"},
	} {
		s.BusinessLine, s.EndUser, s.CreatedAt, s.LastInputAt = "bl", "u", t0, t0
		if err := x.Put(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err := x.MarkClosed(ctx, s.ID, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	_ = x.Put(ctx, &lifecycle.Session{ID: "open", BusinessLine: "bl", EndUser: "u", Agent: "day", CreatedAt: t0, LastInputAt: t0})
	later := t0.Add(48 * time.Hour)
	cases := []struct {
		f            lifecycle.AgentFilter
		idle, closed []string
	}{
		{lifecycle.AgentFilter{Only: "day"}, []string{"open"}, []string{"day1", "day2"}},
		{lifecycle.AgentFilter{Except: []string{"day"}}, nil, []string{"chat", "proj"}},
		{lifecycle.AgentFilter{Except: []string{"day", "project"}}, nil, []string{"chat"}},
		{lifecycle.AgentFilter{Except: []string{}}, []string{"open"}, []string{"chat", "day1", "day2", "proj"}},
		{lifecycle.AgentFilter{Only: "missing"}, nil, nil},
	}
	for _, c := range cases {
		idle, err := x.IdleBefore(ctx, "bl", c.f, later, 10)
		if err != nil || !slices.Equal(idle, c.idle) {
			t.Errorf("idle %+v = %v, %v; want %v", c.f, idle, err, c.idle)
		}
		closed, err := x.ClosedBefore(ctx, "bl", c.f, later, 10)
		if err != nil || !slices.Equal(closed, c.closed) {
			t.Errorf("closed %+v = %v, %v; want %v", c.f, closed, err, c.closed)
		}
	}
}

func RunDeletions(t *testing.T, newDeletions func(t *testing.T) lifecycle.Deletions) {
	ctx := context.Background()
	t.Run("MarkOnceCompleteAndPending", func(t *testing.T) {
		d := newDeletions(t)
		first, err := d.Mark(ctx, &lifecycle.Tombstone{SessionID: "s1", BusinessLine: "bl", Reason: "end_user", RequestedAt: t0})
		if err != nil || !first {
			t.Fatalf("mark = %v, %v", first, err)
		}
		again, _ := d.Mark(ctx, &lifecycle.Tombstone{SessionID: "s1", BusinessLine: "bl", Reason: "retention", RequestedAt: t0.Add(time.Hour)})
		if again {
			t.Fatal("second mark reported as new")
		}
		tb, err := d.Get(ctx, "s1")
		if err != nil || tb.Reason != "end_user" || !tb.RequestedAt.Equal(t0) || !tb.CompletedAt.IsZero() {
			t.Fatalf("get = %+v, %v", tb, err)
		}
		if p, _ := d.Pending(ctx, 10); !slices.Equal(p, []string{"s1"}) {
			t.Fatalf("pending = %v", p)
		}
		_ = d.Complete(ctx, "s1", t0.Add(time.Minute))
		if tb, _ := d.Get(ctx, "s1"); !tb.CompletedAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("completed at = %v", tb.CompletedAt)
		}
		if p, _ := d.Pending(ctx, 10); len(p) != 0 {
			t.Fatalf("pending after completion = %v", p)
		}
		if ok, _ := lifecycle.Deleted(ctx, d, "s1"); !ok {
			t.Fatal("Deleted = false")
		}
		if ok, _ := lifecycle.Deleted(ctx, d, "s2"); ok {
			t.Fatal("Deleted(unknown) = true")
		}
	})

	t.Run("RequestProgress", func(t *testing.T) {
		d := newDeletions(t)
		if err := d.CreateRequest(ctx, &lifecycle.Request{ID: "r1", BusinessLine: "bl", CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
		r, err := d.Request(ctx, "r1")
		if err != nil || r.Sessions != 0 || r.BusinessLine != "bl" {
			t.Fatalf("empty request = %+v, %v", r, err)
		}
		for _, id := range []string{"a", "b", "c"} {
			_, _ = d.Mark(ctx, &lifecycle.Tombstone{SessionID: id, BusinessLine: "bl", Reason: "account_deletion", RequestID: "r1", RequestedAt: t0})
		}
		_, _ = d.Mark(ctx, &lifecycle.Tombstone{SessionID: "z", BusinessLine: "bl", Reason: "end_user", RequestedAt: t0})
		_ = d.Complete(ctx, "b", t0)
		if r, _ := d.Request(ctx, "r1"); r.Sessions != 3 || r.Completed != 1 {
			t.Fatalf("progress = %+v", r)
		}
		if _, err := d.Request(ctx, "nope"); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("missing request: %v", err)
		}
	})

	t.Run("WaitDeletedWakesOnMark", func(t *testing.T) {
		d := newDeletions(t)
		w, ok := d.(lifecycle.Watcher)
		if !ok {
			t.Skip("not a lifecycle.Watcher")
		}
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if err := w.WaitDeleted(short, "s1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait on a live session = %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- w.WaitDeleted(ctx, "s1") }()
		time.Sleep(50 * time.Millisecond) // 等待者已订阅
		if _, err := d.Mark(ctx, &lifecycle.Tombstone{SessionID: "s2", BusinessLine: "bl", Reason: "user", RequestedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Mark(ctx, &lifecycle.Tombstone{SessionID: "s1", BusinessLine: "bl", Reason: "user", RequestedAt: t0}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("waiter not woken by the deletion")
		}
		if err := w.WaitDeleted(ctx, "s1"); err != nil {
			t.Fatalf("wait on an already deleted session = %v", err)
		}
	})
}
