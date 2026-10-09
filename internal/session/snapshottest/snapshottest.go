// Package snapshottest 是 session.Snapshots 实现的一致性测试套件。
package snapshottest

import (
	"context"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/session"
)

func Run(t *testing.T, newStore func(t *testing.T) session.Snapshots) {
	ctx := context.Background()
	snap := func(id string, seq uint64) *v1.SessionSnapshot {
		return &v1.SessionSnapshot{Version: session.ProjectionVersion, SessionId: id, Seq: seq,
			Created: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}, CallIds: []string{"c1"}}
	}
	t.Run("PutKeepsNewestAndDelete", func(t *testing.T) {
		s := newStore(t)
		if got, err := s.Get(ctx, "s1"); err != nil || got != nil {
			t.Fatalf("missing = %v, %v", got, err)
		}
		_ = s.Put(ctx, snap("s1", 10))
		_ = s.Put(ctx, snap("s1", 5)) // 更旧：不覆盖
		_ = s.Put(ctx, snap("s2", 3))
		got, err := s.Get(ctx, "s1")
		if err != nil || got.GetSeq() != 10 || got.GetCreated().GetEndUser() != "u" || len(got.GetCallIds()) != 1 {
			t.Fatalf("get = %v, %v", got, err)
		}
		_ = s.Put(ctx, snap("s1", 12))
		if got, _ := s.Get(ctx, "s1"); got.GetSeq() != 12 {
			t.Fatalf("newer snapshot not stored: %d", got.GetSeq())
		}
		_ = s.Delete(ctx, "s1")
		if got, _ := s.Get(ctx, "s1"); got != nil {
			t.Fatal("deleted snapshot still present")
		}
		if got, _ := s.Get(ctx, "s2"); got.GetSeq() != 3 {
			t.Fatal("delete touched another session")
		}
	})
}
