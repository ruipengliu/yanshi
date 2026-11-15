package memorytest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"yanshi/internal/clock"
	idgen "yanshi/internal/ids"
	"yanshi/internal/memory"
	"yanshi/internal/shard"
)

func storeEndUserKeys(t *testing.T, s memory.Store) {
	ctx := context.Background()
	m := &memory.Memory{ID: "same", BusinessLine: "a", EndUser: "u", Category: memory.Preference, Content: "喜欢骑车", CreatedAt: t0, UpdatedAt: t0, SourceSession: sid("source")}
	if err := s.Put(ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "other", m.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("wrong EndUser read: %v", err)
	}
	if err := s.Supersede(ctx, "other", m.ID, "new", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "other", m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSessionForEndUser(ctx, "other", m.SourceSession); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAccess(ctx, []memory.Access{{MemoryID: m.ID, EndUser: "other", Owner: "a", Reader: "b", SessionID: sid("reader"), RunID: "r", At: t0}}); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Accesses(ctx, "other", "a", t0, 10); err != nil || len(rows) != 0 {
		t.Fatalf("wrong EndUser access recorded: %+v, %v", rows, err)
	}
	if got, err := s.Get(ctx, m.EndUser, m.ID); err != nil || got.SupersededBy != "" || got.Content != m.Content {
		t.Fatalf("wrong EndUser changed memory: %+v, %v", got, err)
	}
	if err := s.DeleteSessionForEndUser(ctx, m.EndUser, m.SourceSession); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, m.EndUser, m.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("owner deletion: %v", err)
	}
}

func grantEndUserKeys(t *testing.T, g memory.Grants) {
	ctx := context.Background()
	grant := &memory.Grant{ID: "same", EndUser: "u", From: "a", To: "b", Categories: []memory.Category{memory.Preference}, CreatedAt: t0}
	if err := g.Create(ctx, grant); err != nil {
		t.Fatal(err)
	}
	other := *grant
	other.EndUser = "other"
	if err := g.Create(ctx, &other); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Get(ctx, "other", grant.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("wrong EndUser read: %v", err)
	}
	if err := g.Revoke(ctx, "other", grant.ID, t0); err != nil {
		t.Fatal(err)
	}
	svc := &memory.Service{Grants: g, Clock: clock.NewFake(t0)}
	if err := svc.RevokeGrant(ctx, "u", "unrelated", grant.ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("wrong business line revoke: %v", err)
	}
	if got, err := g.Get(ctx, "u", grant.ID); err != nil || !got.RevokedAt.IsZero() {
		t.Fatalf("wrong scope revoked grant: %+v, %v", got, err)
	}
	if err := svc.RevokeGrant(ctx, "u", "b", grant.ID); err != nil {
		t.Fatal(err)
	}
}

func saveEndUserIDs(t *testing.T, store memory.Store) {
	ctx := context.Background()
	calls := 0
	svc := &memory.Service{Store: store, Grants: memory.NewMemGrants(), Clock: clock.NewFake(t0), IDs: func() string {
		calls++
		return fmt.Sprintf("token-%d", calls)
	}}
	user := "u"
	other := ""
	for i := 0; i < 100000; i++ {
		candidate := fmt.Sprintf("same-slot-%d", i)
		if shard.Hash(candidate) == shard.Hash(user) {
			other = candidate
			break
		}
	}
	if other == "" {
		t.Fatal("no same-slot fixture")
	}
	base := memory.SaveRequest{BusinessLine: "a", EndUser: user, SessionID: sid("session"), CallID: "external/调用?", Category: memory.Preference, Content: "喜欢骑车"}
	seen := map[string]bool{}
	for i, r := range []memory.SaveRequest{base,
		{BusinessLine: "a", EndUser: other, SessionID: sid("session"), CallID: base.CallID, Category: memory.Preference, Content: "喜欢喝茶"},
		{BusinessLine: "b", EndUser: user, SessionID: sid("session"), CallID: base.CallID, Category: memory.Preference, Content: "常住北京"},
		{BusinessLine: "a", EndUser: user, SessionID: sid("another"), CallID: base.CallID, Category: memory.Preference, Content: "喜欢爬山"},
	} {
		m, err := svc.Save(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if slot, err := idgen.SlotOf(m.ID); err != nil || slot != shard.Hash(r.EndUser) || !strings.HasPrefix(m.ID, "mem_v1_") || seen[m.ID] {
			t.Fatalf("case %d: invalid or shared ID %s, %v", i, m.ID, err)
		}
		seen[m.ID] = true
		again, err := svc.Save(ctx, r)
		if err != nil || again.ID != m.ID || again.EndUser != r.EndUser {
			t.Fatalf("retry: %+v, %v", again, err)
		}
	}
	if calls != 0 {
		t.Fatalf("derived IDs consumed generator %d times", calls)
	}
	random, err := svc.Save(ctx, memory.SaveRequest{BusinessLine: "a", EndUser: user, Category: memory.Plan, Content: "周末去杭州"})
	if err != nil || calls != 1 {
		t.Fatalf("random memory: %+v, %v, generator=%d", random, err, calls)
	}
	grant, err := svc.CreateGrant(ctx, user, "a", "b", []memory.Category{memory.Preference}, time.Time{})
	if err != nil || calls != 2 {
		t.Fatalf("grant: %+v, %v, generator=%d", grant, err, calls)
	}
	for _, id := range []string{random.ID, grant.ID} {
		if slot, err := idgen.SlotOf(id); err != nil || slot != shard.Hash(user) {
			t.Fatalf("random object slot: %s, %v", id, err)
		}
	}
}

// saveRetry：同一调用重试时返回已落盘的原对象，不另建一份；同一 ID 的行若记录的是另一次调用，不能当作重试返回。
func saveRetry(t *testing.T, store memory.Store) {
	ctx := context.Background()
	svc := &memory.Service{Store: store, Clock: clock.NewFake(t0), IDs: idgen.Sequential("unused")}
	r := memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: sid("s"), CallID: "retry/调用", Category: memory.Preference, Content: "喜欢骑车"}
	first, err := svc.Save(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := memory.CallMemoryID(r); first.ID != id {
		t.Fatalf("call-derived ID = %s, want %s", first.ID, id)
	}
	if again, err := svc.Save(ctx, r); err != nil || again.ID != first.ID {
		t.Fatalf("retry: %+v, %v", again, err)
	}
	forged := memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: sid("s"), CallID: "forged/调用", Category: memory.Preference, Content: "喜欢游泳"}
	id, err := memory.CallMemoryID(forged)
	if err != nil {
		t.Fatal(err)
	}
	row := &memory.Memory{ID: id, BusinessLine: forged.BusinessLine, EndUser: forged.EndUser, SourceSession: sid("other"), SourceCall: forged.CallID,
		Category: forged.Category, Content: forged.Content, CreatedAt: t0, UpdatedAt: t0}
	if err := store.Put(ctx, row, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, forged); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("row of another invocation returned as a retry: %v", err)
	}
}
