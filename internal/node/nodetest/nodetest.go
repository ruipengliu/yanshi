// Package nodetest 是 node.Directory 与 node.Inbox 实现的一致性测试套件。
package nodetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/node"
)

func info(id, user, label string) node.Info {
	return node.Info{
		NodeID: id, Scope: node.Scope{BusinessLine: "bl", EndUser: user}, Label: label, Kind: "desktop",
		Capabilities: []*v1.CapabilitySpec{{Name: "read_file", Idempotent: true, Risk: v1.Risk_RISK_LOW}},
	}
}

func RunDirectory(t *testing.T, newDir func(t *testing.T, c clock.Clock) node.Directory) {
	ctx := context.Background()
	setup := func(t *testing.T) node.Directory {
		return newDir(t, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	}

	t.Run("RegisterAndGet", func(t *testing.T) {
		d := setup(t)
		label, _, err := d.Register(ctx, info("n1", "u", "My MacBook"))
		if err != nil || label != "my-macbook" {
			t.Fatalf("register = %q, %v", label, err)
		}
		n, err := d.Get(ctx, "n1")
		if err != nil || !n.Online || n.Label != "my-macbook" || n.Kind != "desktop" ||
			len(n.Capabilities) != 1 || n.Capabilities[0].GetName() != "read_file" || !n.Capabilities[0].GetIdempotent() {
			t.Fatalf("get = %+v, %v", n, err)
		}
		if _, err := d.Get(ctx, "missing"); !errors.Is(err, node.ErrNotFound) {
			t.Fatalf("missing err = %v", err)
		}
	})

	t.Run("LabelsAreUniquePerUser", func(t *testing.T) {
		d := setup(t)
		l1, _, _ := d.Register(ctx, info("n1", "u", "pc"))
		l2, _, _ := d.Register(ctx, info("n2", "u", "pc"))
		l3, _, _ := d.Register(ctx, info("n3", "other", "pc"))
		if l1 != "pc" || l2 != "pc-2" || l3 != "pc" {
			t.Fatalf("labels = %s %s %s", l1, l2, l3)
		}
		again, _, _ := d.Register(ctx, info("n1", "u", "pc"))
		if again != "pc" {
			t.Fatalf("re-register changed label to %s", again)
		}
	})

	t.Run("NodeCannotChangeScope", func(t *testing.T) {
		d := setup(t)
		_, _, _ = d.Register(ctx, info("n1", "u", "pc"))
		if _, _, err := d.Register(ctx, info("n1", "intruder", "pc")); err == nil {
			t.Fatal("node moved to another user")
		}
	})

	t.Run("StaleDisconnectDoesNotMarkOffline", func(t *testing.T) {
		d := setup(t)
		_, g1, _ := d.Register(ctx, info("n1", "u", "pc"))
		_, g2, _ := d.Register(ctx, info("n1", "u", "pc"))
		if err := d.SetOffline(ctx, "n1", g1); err != nil {
			t.Fatal(err)
		}
		if n, _ := d.Get(ctx, "n1"); !n.Online {
			t.Fatal("stale connection marked node offline")
		}
		_ = d.SetOffline(ctx, "n1", g2)
		if n, _ := d.Get(ctx, "n1"); n.Online {
			t.Fatal("current connection did not mark offline")
		}
	})

	t.Run("DeleteScope", func(t *testing.T) {
		d := setup(t)
		_, _, _ = d.Register(ctx, info("n1", "u", "a"))
		_, _, _ = d.Register(ctx, info("n2", "u", "b"))
		_, _, _ = d.Register(ctx, info("n3", "v", "a"))
		ids, err := d.DeleteScope(ctx, node.Scope{BusinessLine: "bl", EndUser: "u"})
		if err != nil || len(ids) != 2 || ids[0] != "n1" || ids[1] != "n2" {
			t.Fatalf("deleted = %v, %v", ids, err)
		}
		if _, err := d.Get(ctx, "n1"); !errors.Is(err, node.ErrNotFound) {
			t.Fatal("deleted node still registered")
		}
		if _, err := d.Get(ctx, "n3"); err != nil {
			t.Fatal("delete touched another user")
		}
	})

	t.Run("ListByScopeSortedByLabel", func(t *testing.T) {
		d := setup(t)
		_, _, _ = d.Register(ctx, info("n1", "u", "phone"))
		_, _, _ = d.Register(ctx, info("n2", "u", "desktop"))
		_, _, _ = d.Register(ctx, info("n3", "other", "x"))
		ns, err := d.List(ctx, node.Scope{BusinessLine: "bl", EndUser: "u"})
		if err != nil || len(ns) != 2 || ns[0].Label != "desktop" || ns[1].Label != "phone" {
			t.Fatalf("list = %+v, %v", ns, err)
		}
	})
}

func RunInbox(t *testing.T, newInbox func(t *testing.T) node.Inbox) {
	ctx := context.Background()
	inv := func(id string) *v1.Invoke {
		return &v1.Invoke{SessionId: "s", RunId: "r", CallId: id, Capability: "read_file"}
	}
	ids := func(items []*v1.Invoke) []string {
		var out []string
		for _, i := range items {
			out = append(out, i.GetCallId())
		}
		return out
	}

	t.Run("PutIsIdempotentAndOrdered", func(t *testing.T) {
		in := newInbox(t)
		_ = in.Put(ctx, "n", inv("a"))
		_ = in.Put(ctx, "n", inv("b"))
		_, v1, _ := in.Pending(ctx, "n")
		_ = in.Put(ctx, "n", inv("a"))
		items, v2, err := in.Pending(ctx, "n")
		if err != nil || len(items) != 2 || items[0].GetCallId() != "a" || items[1].GetCallId() != "b" {
			t.Fatalf("pending = %v, %v", ids(items), err)
		}
		if v1 != v2 {
			t.Fatal("duplicate put changed version")
		}
		if items[0].GetCapability() != "read_file" || items[0].GetSessionId() != "s" {
			t.Fatalf("item = %v", items[0])
		}
	})

	t.Run("RemoveAndNodesIndependent", func(t *testing.T) {
		in := newInbox(t)
		_ = in.Put(ctx, "n1", inv("a"))
		_ = in.Put(ctx, "n2", inv("b"))
		_ = in.Remove(ctx, "n1", "a")
		_ = in.Remove(ctx, "n1", "missing")
		if items, _, _ := in.Pending(ctx, "n1"); len(items) != 0 {
			t.Fatalf("n1 = %v", ids(items))
		}
		if items, _, _ := in.Pending(ctx, "n2"); len(items) != 1 {
			t.Fatalf("n2 = %v", ids(items))
		}
	})

	t.Run("RemoveSessionAcrossNodes", func(t *testing.T) {
		b := newInbox(t)
		put := func(nodeID, sid, call string) { _ = b.Put(ctx, nodeID, &v1.Invoke{SessionId: sid, CallId: call}) }
		put("n1", "s1", "c1")
		put("n1", "s2", "c2")
		put("n2", "s1", "c3")
		_, v1Before, _ := b.Pending(ctx, "n1")
		if err := b.RemoveSession(ctx, "s1"); err != nil {
			t.Fatal(err)
		}
		p1, v1After, _ := b.Pending(ctx, "n1")
		p2, _, _ := b.Pending(ctx, "n2")
		if len(p1) != 1 || p1[0].GetCallId() != "c2" || len(p2) != 0 {
			t.Fatalf("pending n1 = %v, n2 = %v", p1, p2)
		}
		if v1After == v1Before {
			t.Fatal("version not bumped: watchers would not see the withdrawal")
		}
	})

	t.Run("WaitWakesOnChange", func(t *testing.T) {
		in := newInbox(t)
		_, v, _ := in.Pending(ctx, "n")
		var wg sync.WaitGroup
		var werr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			werr = in.Wait(c, "n", v)
		}()
		time.Sleep(20 * time.Millisecond)
		_ = in.Put(ctx, "n", inv("a"))
		wg.Wait()
		if werr != nil {
			t.Fatalf("wait = %v", werr)
		}
		if err := in.Wait(ctx, "n", v); err != nil {
			t.Fatalf("wait on stale version should return at once: %v", err)
		}
		c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		_, cur, _ := in.Pending(ctx, "n")
		if err := in.Wait(c, "n", cur); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait without change = %v", err)
		}
	})
}
