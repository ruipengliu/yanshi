// Package nodetest 是 node.Directory 与 node.Inbox 实现的一致性测试套件。
package nodetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

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
		n, err := d.Get(ctx, "u", "n1")
		if err != nil || !n.Online || n.Label != "my-macbook" || n.Kind != "desktop" ||
			len(n.Capabilities) != 1 || n.Capabilities[0].GetName() != "read_file" || !n.Capabilities[0].GetIdempotent() {
			t.Fatalf("get = %+v, %v", n, err)
		}
		if _, err := d.Get(ctx, "u", "missing"); !errors.Is(err, node.ErrNotFound) {
			t.Fatalf("missing err = %v", err)
		}
	})

	t.Run("SameNodeIDAcrossEndUsers", func(t *testing.T) {
		d := setup(t)
		users := []string{"alice", "bob"}
		gens := make([]uint64, len(users))
		errs := make([]error, len(users))
		var wg sync.WaitGroup
		for i, user := range users {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, gens[i], errs[i] = d.Register(ctx, info("same", user, user))
			}()
		}
		wg.Wait()
		for i, user := range users {
			if errs[i] != nil || gens[i] != 1 {
				t.Fatalf("register %s: generation %d, %v", user, gens[i], errs[i])
			}
			n, err := d.Get(ctx, user, "same")
			if err != nil || n.Scope.EndUser != user || n.Label != user || !n.Online {
				t.Fatalf("get %s: %+v, %v", user, n, err)
			}
		}
		_, gen, err := d.Register(ctx, info("same", "alice", "new-label"))
		if err != nil || gen != 2 {
			t.Fatalf("reconnect: generation %d, %v", gen, err)
		}
		if err := d.SetOffline(ctx, "alice", "same", gens[0]); err != nil {
			t.Fatal(err)
		}
		if n, err := d.Get(ctx, "alice", "same"); err != nil || !n.Online {
			t.Fatalf("old connection affected reconnect: %+v, %v", n, err)
		}
		if err := d.SetOffline(ctx, "alice", "same", gen); err != nil {
			t.Fatal(err)
		}
		if n, err := d.Get(ctx, "bob", "same"); err != nil || !n.Online || n.Label != "bob" {
			t.Fatalf("other user affected: %+v, %v", n, err)
		}
		if _, err := d.DeleteScope(ctx, node.Scope{BusinessLine: "bl", EndUser: "alice"}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Get(ctx, "alice", "same"); !errors.Is(err, node.ErrNotFound) {
			t.Fatalf("deleted node: %v", err)
		}
		if _, err := d.Get(ctx, "bob", "same"); err != nil {
			t.Fatal(err)
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
		other := info("n1", "u", "pc")
		other.Scope.BusinessLine = "other"
		if _, _, err := d.Register(ctx, other); err == nil {
			t.Fatal("node moved to another business line")
		}
	})

	// 同一 EndUser 的两个业务线并发首次注册同一个 ID：至多一方成功。
	// 前置的归属检查锁不住尚不存在的记录，归属必须在写入本身中判定。
	t.Run("ConcurrentFirstRegistrationHasOneOwner", func(t *testing.T) {
		d := setup(t)
		for i := range 20 {
			id := fmt.Sprintf("race%d", i)
			lines := []string{"bl", "other"}
			errs := make([]error, len(lines))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for j, bl := range lines {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					candidate := info(id, "u", "pc")
					candidate.Scope.BusinessLine = bl
					_, _, errs[j] = d.Register(ctx, candidate)
				}()
			}
			close(start)
			wg.Wait()
			n, err := d.Get(ctx, "u", id)
			if err != nil {
				t.Fatal(err)
			}
			if (errs[0] == nil) == (errs[1] == nil) {
				t.Fatalf("expected one scope to register: %v", errs)
			}
			for j, bl := range lines {
				if errs[j] == nil && n.Scope.BusinessLine != bl {
					t.Fatalf("%s: %s registered but the node belongs to %s", id, bl, n.Scope.BusinessLine)
				}
			}
		}
	})

	t.Run("StaleDisconnectDoesNotMarkOffline", func(t *testing.T) {
		d := setup(t)
		_, g1, _ := d.Register(ctx, info("n1", "u", "pc"))
		_, g2, _ := d.Register(ctx, info("n1", "u", "pc"))
		if err := d.SetOffline(ctx, "u", "n1", g1); err != nil {
			t.Fatal(err)
		}
		if n, _ := d.Get(ctx, "u", "n1"); !n.Online {
			t.Fatal("stale connection marked node offline")
		}
		_ = d.SetOffline(ctx, "u", "n1", g2)
		if n, _ := d.Get(ctx, "u", "n1"); n.Online {
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
		if _, err := d.Get(ctx, "u", "n1"); !errors.Is(err, node.ErrNotFound) {
			t.Fatal("deleted node still registered")
		}
		if _, err := d.Get(ctx, "v", "n3"); err != nil {
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

	t.Run("SameNodeAndCallIDsAcrossEndUsers", func(t *testing.T) {
		in := newInbox(t)
		for _, user := range []string{"alice", "bob"} {
			i := inv("same-call")
			i.SessionId = user + "-session"
			if err := in.Put(ctx, user, "same-node", i); err != nil {
				t.Fatal(err)
			}
		}
		_, bobVersion, err := in.Pending(ctx, "bob", "same-node")
		if err != nil {
			t.Fatal(err)
		}
		if err := in.Remove(ctx, "alice", "same-node", "same-call"); err != nil {
			t.Fatal(err)
		}
		if items, _, err := in.Pending(ctx, "alice", "same-node"); err != nil || len(items) != 0 {
			t.Fatalf("alice: %v, %v", items, err)
		}
		items, version, err := in.Pending(ctx, "bob", "same-node")
		if err != nil || len(items) != 1 || items[0].GetSessionId() != "bob-session" || version != bobVersion {
			t.Fatalf("bob: %v, version %d/%d, %v", items, version, bobVersion, err)
		}
		// 已知 EndUser 时仅处理该用户，即使别的用户的副本也写了同一 Session。
		if err := in.RemoveSession(ctx, "alice", "bob-session"); err != nil {
			t.Fatal(err)
		}
		if items, _, err := in.Pending(ctx, "bob", "same-node"); err != nil || len(items) != 1 {
			t.Fatalf("scoped sweep crossed users: %v, %v", items, err)
		}
		if err := in.RemoveSession(ctx, "bob", "bob-session"); err != nil {
			t.Fatal(err)
		}
		if items, _, err := in.Pending(ctx, "bob", "same-node"); err != nil || len(items) != 0 {
			t.Fatalf("scoped sweep left calls: %v, %v", items, err)
		}
	})

	t.Run("RemoveSessionRecoversEndUsers", func(t *testing.T) {
		in := newInbox(t)
		for _, user := range []string{"alice", "bob"} {
			if err := in.Put(ctx, user, "n", inv("a")); err != nil {
				t.Fatal(err)
			}
			if err := in.Put(ctx, user, "n", &v1.Invoke{SessionId: "keep", CallId: "b"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := in.RemoveSession(ctx, "", "s"); err != nil {
			t.Fatal(err)
		}
		for _, user := range []string{"alice", "bob"} {
			items, version, err := in.Pending(ctx, user, "n")
			if err != nil || len(items) != 1 || items[0].GetSessionId() != "keep" || version != 3 {
				t.Fatalf("sweep %s: %v, version %d, %v", user, items, version, err)
			}
		}
	})

	t.Run("WaitIsolatedByEndUser", func(t *testing.T) {
		in := newInbox(t)
		// 包含分隔符与数字结尾，防止复合通知键被歧义拆分或被误当作版本。
		user, other, id := "a:b", "a", "node:12"
		_, version, err := in.Pending(ctx, user, id)
		if err != nil {
			t.Fatal(err)
		}
		c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- in.Wait(c, user, id, version) }()
		if err := in.Put(ctx, other, id, inv("a")); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("another end user woke Wait: %v", err)
		}
		if err := in.Put(ctx, user, id, inv("a")); err != nil {
			t.Fatal(err)
		}
		c2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		defer cancel2()
		if err := in.Wait(c2, user, id, version); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("PutIsIdempotentAndOrdered", func(t *testing.T) {
		in := newInbox(t)
		_ = in.Put(ctx, "u", "n", inv("a"))
		_ = in.Put(ctx, "u", "n", inv("b"))
		_, v1, _ := in.Pending(ctx, "u", "n")
		_ = in.Put(ctx, "u", "n", inv("a"))
		items, v2, err := in.Pending(ctx, "u", "n")
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

	t.Run("PutExtendsDeadline", func(t *testing.T) {
		in := newInbox(t)
		at := func(sec int64) *v1.Invoke {
			i := inv("a")
			i.Deadline = timestamppb.New(time.Unix(sec, 0))
			return i
		}
		_ = in.Put(ctx, "u", "n", at(100))
		_ = in.Put(ctx, "u", "n", inv("b"))
		_, v0, _ := in.Pending(ctx, "u", "n")
		if err := in.Put(ctx, "u", "n", at(200)); err != nil {
			t.Fatal(err)
		}
		items, v1, _ := in.Pending(ctx, "u", "n")
		if len(items) != 2 || items[0].GetCallId() != "a" || items[0].GetDeadline().GetSeconds() != 200 {
			t.Fatalf("pending = %v", items)
		}
		if v1 == v0 {
			t.Fatal("extending a deadline did not bump the version")
		}
		_ = in.Put(ctx, "u", "n", at(150)) // 过时的派发不把截止时间改回去
		items, v2, _ := in.Pending(ctx, "u", "n")
		if items[0].GetDeadline().GetSeconds() != 200 || v2 != v1 {
			t.Fatalf("stale put changed the item: %v (version %d → %d)", items[0], v1, v2)
		}
	})

	t.Run("RemoveAndNodesIndependent", func(t *testing.T) {
		in := newInbox(t)
		_ = in.Put(ctx, "u", "n1", inv("a"))
		_ = in.Put(ctx, "u", "n2", inv("b"))
		_ = in.Remove(ctx, "u", "n1", "a")
		_ = in.Remove(ctx, "u", "n1", "missing")
		if items, _, _ := in.Pending(ctx, "u", "n1"); len(items) != 0 {
			t.Fatalf("n1 = %v", ids(items))
		}
		if items, _, _ := in.Pending(ctx, "u", "n2"); len(items) != 1 {
			t.Fatalf("n2 = %v", ids(items))
		}
	})

	t.Run("RemoveSessionAcrossNodes", func(t *testing.T) {
		b := newInbox(t)
		put := func(nodeID, sid, call string) { _ = b.Put(ctx, "u", nodeID, &v1.Invoke{SessionId: sid, CallId: call}) }
		put("n1", "s1", "c1")
		put("n1", "s2", "c2")
		put("n2", "s1", "c3")
		_, v1Before, _ := b.Pending(ctx, "u", "n1")
		if err := b.RemoveSession(ctx, "u", "s1"); err != nil {
			t.Fatal(err)
		}
		p1, v1After, _ := b.Pending(ctx, "u", "n1")
		p2, _, _ := b.Pending(ctx, "u", "n2")
		if len(p1) != 1 || p1[0].GetCallId() != "c2" || len(p2) != 0 {
			t.Fatalf("pending n1 = %v, n2 = %v", p1, p2)
		}
		if v1After == v1Before {
			t.Fatal("version not bumped: watchers would not see the withdrawal")
		}
	})

	t.Run("WaitWakesOnChange", func(t *testing.T) {
		in := newInbox(t)
		_, v, _ := in.Pending(ctx, "u", "n")
		var wg sync.WaitGroup
		var werr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			werr = in.Wait(c, "u", "n", v)
		}()
		time.Sleep(20 * time.Millisecond)
		_ = in.Put(ctx, "u", "n", inv("a"))
		wg.Wait()
		if werr != nil {
			t.Fatalf("wait = %v", werr)
		}
		if err := in.Wait(ctx, "u", "n", v); err != nil {
			t.Fatalf("wait on stale version should return at once: %v", err)
		}
		c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		_, cur, _ := in.Pending(ctx, "u", "n")
		if err := in.Wait(c, "u", "n", cur); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait without change = %v", err)
		}
	})
}
