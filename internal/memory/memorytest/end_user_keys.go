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
	m := &memory.Memory{ID: "same", BusinessLine: "a", EndUser: "u", Category: memory.Preference, Content: "喜欢骑车", CreatedAt: t0, UpdatedAt: t0, SourceSession: "source"}
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
	if err := s.RecordAccess(ctx, []memory.Access{{MemoryID: m.ID, EndUser: "other", Owner: "a", Reader: "b", SessionID: "reader", RunID: "r", At: t0}}); err != nil {
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
	base := memory.SaveRequest{BusinessLine: "a", EndUser: user, SessionID: "session", CallID: "external/调用?", Category: memory.Preference, Content: "喜欢骑车"}
	seen := map[string]bool{}
	for i, r := range []memory.SaveRequest{base,
		{BusinessLine: "a", EndUser: other, SessionID: "session", CallID: base.CallID, Category: memory.Preference, Content: "喜欢喝茶"},
		{BusinessLine: "b", EndUser: user, SessionID: "session", CallID: base.CallID, Category: memory.Preference, Content: "常住北京"},
		{BusinessLine: "a", EndUser: user, SessionID: "another", CallID: base.CallID, Category: memory.Preference, Content: "喜欢爬山"},
	} {
		m, err := svc.Save(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if slot, legacy, err := idgen.SlotOf(m.ID); err != nil || legacy || slot != shard.Hash(r.EndUser) || !strings.HasPrefix(m.ID, "mem_v1_") || seen[m.ID] {
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
		if slot, legacy, err := idgen.SlotOf(id); err != nil || legacy || slot != shard.Hash(user) {
			t.Fatalf("random object slot: %s, %v", id, err)
		}
	}
}

func saveLegacyRetry(t *testing.T, store memory.Store) {
	ctx := context.Background()
	r := memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: "s", CallID: "legacy/调用", Category: memory.Preference, Content: "喜欢骑车"}
	old := &memory.Memory{ID: "mem_" + r.CallID, BusinessLine: r.BusinessLine, EndUser: r.EndUser, SourceSession: r.SessionID, SourceCall: r.CallID,
		Category: r.Category, Content: r.Content, CreatedAt: t0, UpdatedAt: t0}
	if err := store.Put(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	svc := &memory.Service{Store: store, Clock: clock.NewFake(t0), IDs: idgen.Sequential("unused")}
	got, err := svc.Save(ctx, r)
	if err != nil || got.ID != old.ID {
		t.Fatalf("legacy retry: %+v, %v", got, err)
	}
	id, _ := memory.CallMemoryID(r)
	if _, err := store.Get(ctx, r.EndUser, id); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("legacy retry created new object: %v", err)
	}
	// 即使同时存在新格式行，旧派生也优先，不能让升级改变在途调用的原对象。
	newRow := *old
	newRow.ID = id
	if err := store.Put(ctx, &newRow, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Save(ctx, r); err != nil || got.ID != old.ID {
		t.Fatalf("legacy priority: %+v, %v", got, err)
	}
	for _, change := range []func(*memory.SaveRequest){
		func(r *memory.SaveRequest) { r.BusinessLine = "b" },
		func(r *memory.SaveRequest) { r.SessionID = "other" },
	} {
		bad := r
		change(&bad)
		if _, err := svc.Save(ctx, bad); !errors.Is(err, memory.ErrRejected) {
			t.Fatalf("legacy ownership mismatch accepted: %v", err)
		}
	}
	// 另一个用户读不到旧行；同一个外部 call_id 在自己的命名空间创建新对象。
	r.EndUser = "another-user"
	got, err = svc.Save(ctx, r)
	if err != nil || got.ID == old.ID || got.EndUser != r.EndUser {
		t.Fatalf("cross-user call ID: %+v, %v", got, err)
	}
}
