// Package memorytest 是 memory.Store 与 memory.Grants 实现的一致性测试套件。
package memorytest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"yanshi/internal/memory"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func mem(id, bl, eu string, c memory.Category, content, session string, at time.Time) *memory.Memory {
	return &memory.Memory{ID: id, BusinessLine: bl, EndUser: eu, Category: c, Content: content,
		SourceSession: session, SourceCall: "call_" + id, CreatedAt: at, UpdatedAt: at}
}

func ids(ms []*memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func RunStore(t *testing.T, newStore func(t *testing.T) memory.Store) {
	ctx := context.Background()
	all := []memory.Scope{{BusinessLine: "a"}, {BusinessLine: "b"}}

	t.Run("PutGetOverwriteDelete", func(t *testing.T) {
		s := newStore(t)
		m := mem("m1", "a", "u", memory.Preference, "偏好简洁的回答", "s1", t0)
		if err := s.Put(ctx, m, nil); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "m1")
		if err != nil || got.Content != m.Content || got.Category != memory.Preference || got.SourceSession != "s1" ||
			got.SourceCall != "call_m1" || !got.CreatedAt.Equal(t0) {
			t.Fatalf("get = %+v, %v", got, err)
		}
		m.Content = "偏好详细的回答"
		_ = s.Put(ctx, m, nil)
		if got, _ := s.Get(ctx, "m1"); got.Content != "偏好详细的回答" {
			t.Fatalf("overwrite: %q", got.Content)
		}
		_ = s.Delete(ctx, "m1")
		if _, err := s.Get(ctx, "m1"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
		if err := s.Delete(ctx, "m1"); err != nil {
			t.Fatalf("delete is not idempotent: %v", err)
		}
	})

	t.Run("ListRespectsScopesAndOrder", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, mem("a1", "a", "u", memory.Preference, "x", "s", t0), nil)
		_ = s.Put(ctx, mem("a2", "a", "u", memory.Relationship, "y", "s", t0.Add(time.Minute)), nil)
		_ = s.Put(ctx, mem("b1", "b", "u", memory.Preference, "z", "s", t0.Add(2*time.Minute)), nil)
		_ = s.Put(ctx, mem("b2", "b", "u", memory.Relationship, "w", "s", t0.Add(3*time.Minute)), nil)
		_ = s.Put(ctx, mem("other", "a", "v", memory.Preference, "q", "s", t0), nil)
		got, _ := s.List(ctx, "u", all, 10)
		if !slices.Equal(ids(got), []string{"b2", "b1", "a2", "a1"}) {
			t.Fatalf("all scopes = %v", ids(got))
		}
		limited := []memory.Scope{{BusinessLine: "a"}, {BusinessLine: "b", Categories: []memory.Category{memory.Preference}}}
		got, _ = s.List(ctx, "u", limited, 10)
		if !slices.Equal(ids(got), []string{"b1", "a2", "a1"}) {
			t.Fatalf("category-limited scope = %v", ids(got))
		}
		got, _ = s.List(ctx, "u", all, 2)
		if len(got) != 2 {
			t.Fatalf("limit: %d", len(got))
		}
		if n, _ := s.Count(ctx, "a", "u"); n != 2 {
			t.Fatalf("count = %d", n)
		}
	})

	t.Run("NearestUsesSameModelOnly", func(t *testing.T) {
		s := newStore(t)
		put := func(id string, v []float32, model string) {
			if err := s.Put(ctx, mem(id, "a", "u", memory.Interest, id, "s", t0), &memory.Vector{Model: model, Values: v}); err != nil {
				t.Fatal(err)
			}
		}
		put("close", []float32{1, 0.1, 0}, "m1")
		put("far", []float32{0, 1, 0}, "m1")
		put("mid", []float32{1, 1, 0}, "m1")
		put("othermodel", []float32{1, 0, 0}, "m2")
		_ = s.Put(ctx, mem("novec", "a", "u", memory.Interest, "novec", "s", t0), nil)
		hits, err := s.Nearest(ctx, "u", all, memory.Vector{Model: "m1", Values: []float32{1, 0, 0}}, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range hits {
			got = append(got, h.ID)
		}
		if !slices.Equal(got, []string{"close", "mid", "far"}) {
			t.Fatalf("nearest = %v", got)
		}
		if hits[0].Score < 0.99 || hits[2].Score > 0.01 {
			t.Fatalf("scores = %v, %v", hits[0].Score, hits[2].Score)
		}
	})

	t.Run("DeleteBySessionAndEndUser", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, mem("x1", "a", "u", memory.Plan, "p", "s1", t0), &memory.Vector{Model: "m", Values: []float32{1}})
		_ = s.Put(ctx, mem("x2", "a", "u", memory.Plan, "p", "s2", t0), nil)
		_ = s.Put(ctx, mem("x3", "b", "u", memory.Plan, "p", "s3", t0), nil)
		_ = s.Put(ctx, mem("x4", "a", "v", memory.Plan, "p", "s4", t0), nil)
		_ = s.DeleteSession(ctx, "s1")
		if _, err := s.Get(ctx, "x1"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatal("memory of deleted session remains")
		}
		_ = s.DeleteEndUser(ctx, "a", "u")
		for id, want := range map[string]bool{"x2": false, "x3": true, "x4": true} {
			_, err := s.Get(ctx, id)
			if (err == nil) != want {
				t.Errorf("%s present = %v, want %v", id, err == nil, want)
			}
		}
	})
}

func RunAccess(t *testing.T, newStore func(t *testing.T) memory.Store) {
	ctx := context.Background()
	t.Run("RecordListAndDeleteWithMemory", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, mem("m1", "a", "u", memory.Preference, "x", "s1", t0), nil)
		_ = s.Put(ctx, mem("m2", "a", "u", memory.Preference, "y", "s1", t0), nil)
		acc := func(id, reader string, at time.Time) memory.Access {
			return memory.Access{MemoryID: id, Owner: "a", EndUser: "u", Reader: reader, SessionID: "sr", RunID: "r", At: at}
		}
		err := s.RecordAccess(ctx, []memory.Access{acc("m1", "a", t0), acc("m1", "b", t0.Add(time.Minute)), acc("m2", "b", t0.Add(2*time.Minute)),
			acc("missing", "b", t0)})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := s.Accesses(ctx, "u", "a", t0.Add(30*time.Second), 10)
		if len(got) != 2 || got[0].MemoryID != "m2" || got[1].Reader != "b" || !got[0].At.Equal(t0.Add(2*time.Minute)) {
			t.Fatalf("accesses = %+v", got)
		}
		_ = s.Delete(ctx, "m1")
		if got, _ := s.Accesses(ctx, "u", "a", t0, 10); len(got) != 1 || got[0].MemoryID != "m2" {
			t.Fatalf("after deleting m1: %+v", got)
		}
		_ = s.DeleteSession(ctx, "s1")
		if got, _ := s.Accesses(ctx, "u", "a", t0, 10); len(got) != 0 {
			t.Fatalf("accesses outlived their memory: %+v", got)
		}
	})
}

func RunGrants(t *testing.T, newGrants func(t *testing.T) memory.Grants) {
	ctx := context.Background()
	t.Run("CreateRevokeList", func(t *testing.T) {
		g := newGrants(t)
		for i, to := range []string{"b", "c"} {
			if err := g.Create(ctx, &memory.Grant{ID: fmt.Sprintf("g%d", i), EndUser: "u", From: "a", To: to,
				Categories: []memory.Category{memory.Preference, memory.Interest}, CreatedAt: t0.Add(time.Duration(i) * time.Minute),
				ExpiresAt: t0.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		_ = g.Create(ctx, &memory.Grant{ID: "other", EndUser: "v", From: "a", To: "b", Categories: []memory.Category{memory.Plan}, CreatedAt: t0})
		got, err := g.Get(ctx, "g0")
		if err != nil || got.To != "b" || !slices.Equal(got.Categories, []memory.Category{memory.Preference, memory.Interest}) ||
			!got.ExpiresAt.Equal(t0.Add(time.Hour)) || !got.RevokedAt.IsZero() {
			t.Fatalf("get = %+v, %v", got, err)
		}
		_ = g.Revoke(ctx, "g0", t0.Add(10*time.Minute))
		_ = g.Revoke(ctx, "g0", t0.Add(20*time.Minute)) // 已撤销：不变
		if got, _ := g.Get(ctx, "g0"); !got.RevokedAt.Equal(t0.Add(10*time.Minute)) || got.ActiveAt(t0.Add(11*time.Minute)) {
			t.Fatalf("revoked = %+v", got)
		}
		list, _ := g.List(ctx, "u", "a")
		if len(list) != 2 || list[0].ID != "g1" {
			t.Fatalf("list = %d, first %s", len(list), list[0].ID)
		}
		if r, _ := g.ToReader(ctx, "u", "b"); len(r) != 1 || r[0].ID != "g0" {
			t.Fatalf("to reader = %v", r)
		}
		if _, err := g.Get(ctx, "nope"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})

	t.Run("DeleteBusinessLine", func(t *testing.T) {
		g := newGrants(t)
		_ = g.Create(ctx, &memory.Grant{ID: "ab", EndUser: "u", From: "a", To: "b", Categories: []memory.Category{memory.Plan}, CreatedAt: t0})
		_ = g.Create(ctx, &memory.Grant{ID: "ca", EndUser: "u", From: "c", To: "a", Categories: []memory.Category{memory.Plan}, CreatedAt: t0})
		_ = g.Create(ctx, &memory.Grant{ID: "bc", EndUser: "u", From: "b", To: "c", Categories: []memory.Category{memory.Plan}, CreatedAt: t0})
		_ = g.DeleteBusinessLine(ctx, "u", "a")
		for id, want := range map[string]bool{"ab": false, "ca": false, "bc": true} {
			_, err := g.Get(ctx, id)
			if (err == nil) != want {
				t.Errorf("%s present = %v, want %v", id, err == nil, want)
			}
		}
	})
}
