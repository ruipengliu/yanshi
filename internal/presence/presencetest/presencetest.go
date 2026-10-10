// Package presencetest 是 presence.Store 实现的一致性测试套件。
package presencetest

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/presence"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func entry(sid, conn string) *presence.Entry {
	return &presence.Entry{SessionID: sid, ConnID: conn, DeviceID: "dev-" + conn, Label: "phone", Kind: "mobile",
		Expires: t0.Add(presence.TTL)}
}

func list(t *testing.T, s presence.Store, sid string, now time.Time) ([]*presence.Entry, uint64) {
	t.Helper()
	es, v, err := s.List(context.Background(), sid, now)
	if err != nil {
		t.Fatal(err)
	}
	return es, v
}

func Run(t *testing.T, newStore func(t *testing.T) presence.Store) {
	ctx := context.Background()

	t.Run("VersionChangesOnlyWithVisibleContent", func(t *testing.T) {
		s := newStore(t)
		_, v0 := list(t, s, "s1", t0)
		e := entry("s1", "c1")
		if err := s.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
		es, v1 := list(t, s, "s1", t0)
		if len(es) != 1 || es[0].DeviceID != "dev-c1" || es[0].Label != "phone" || v1 == v0 {
			t.Fatalf("after put: %+v version %d→%d", es, v0, v1)
		}
		// 只改有效期（续期）：版本不变。
		e.Expires = t0.Add(2 * presence.TTL)
		if err := s.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
		if _, v := list(t, s, "s1", t0); v != v1 {
			t.Fatal("renewing an entry must not change the version")
		}
		if err := s.Touch(ctx, "c1", t0.Add(3*presence.TTL)); err != nil {
			t.Fatal(err)
		}
		es, v := list(t, s, "s1", t0.Add(2*presence.TTL+time.Second))
		if v != v1 || len(es) != 1 || !es[0].Expires.Equal(t0.Add(3*presence.TTL)) {
			t.Fatalf("touch: %+v version %d (was %d)", es, v, v1)
		}
		// 改显示内容：版本变化。
		e.Focused, e.TypingUntil = true, t0.Add(presence.TypingFor)
		if err := s.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
		es, v2 := list(t, s, "s1", t0)
		if v2 == v1 || !es[0].Focused || !es[0].TypingUntil.Equal(t0.Add(presence.TypingFor)) {
			t.Fatalf("visible change: %+v version %d (was %d)", es, v2, v1)
		}
	})

	t.Run("ExpiredEntriesAreNotListed", func(t *testing.T) {
		s := newStore(t)
		a, b := entry("s1", "a"), entry("s1", "b")
		b.Expires = t0.Add(time.Second)
		for _, e := range []*presence.Entry{a, b} {
			if err := s.Put(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		if es, _ := list(t, s, "s1", t0); len(es) != 2 || es[0].ConnID != "a" || es[1].ConnID != "b" {
			t.Fatalf("list = %+v, want a and b in conn order", es)
		}
		if es, _ := list(t, s, "s1", t0.Add(time.Second)); len(es) != 1 || es[0].ConnID != "a" {
			t.Fatalf("expired entry listed: %+v", es)
		}
		if err := s.Prune(ctx, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if es, _ := list(t, s, "s1", t0); len(es) != 1 || es[0].ConnID != "a" {
			t.Fatalf("after prune: %+v", es)
		}
	})

	t.Run("RemoveAndIsolation", func(t *testing.T) {
		s := newStore(t)
		for _, e := range []*presence.Entry{entry("s1", "a"), entry("s2", "a")} {
			if err := s.Put(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		_, v := list(t, s, "s1", t0)
		if err := s.Remove(ctx, "s1", "missing"); err != nil {
			t.Fatal(err)
		}
		if _, v2 := list(t, s, "s1", t0); v2 != v {
			t.Fatal("removing a missing entry changed the version")
		}
		if err := s.Remove(ctx, "s1", "a"); err != nil {
			t.Fatal(err)
		}
		if es, v2 := list(t, s, "s1", t0); len(es) != 0 || v2 == v {
			t.Fatalf("after remove: %+v version %d (was %d)", es, v2, v)
		}
		if es, _ := list(t, s, "s2", t0); len(es) != 1 {
			t.Fatal("removing from one session affected another")
		}
	})

	t.Run("WaitWakesOnChange", func(t *testing.T) {
		s := newStore(t)
		_, v := list(t, s, "s1", t0)
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if err := s.Wait(short, "s1", v); err == nil {
			t.Fatal("wait returned without a change")
		}
		done := make(chan error, 1)
		go func() { done <- s.Wait(ctx, "s1", v) }()
		time.Sleep(20 * time.Millisecond)
		if err := s.Put(ctx, entry("s1", "c")); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("wait did not wake on a change")
		}
	})

	t.Run("DeleteSession", func(t *testing.T) {
		s := newStore(t)
		for _, e := range []*presence.Entry{entry("s1", "a"), entry("s1", "b"), entry("s2", "a")} {
			if err := s.Put(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		_, v := list(t, s, "s1", t0)
		done := make(chan error, 1)
		go func() { done <- s.Wait(ctx, "s1", v) }()
		if err := s.DeleteSession(ctx, "s1"); err != nil {
			t.Fatal(err)
		}
		if es, _ := list(t, s, "s1", t0); len(es) != 0 {
			t.Fatalf("entries left after deleting the session: %+v", es)
		}
		if es, _ := list(t, s, "s2", t0); len(es) != 1 {
			t.Fatal("deleting one session affected another")
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("deleting the session did not wake waiters")
		}
	})
}
