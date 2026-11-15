package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
)

func TestEndUserBarrierWithdrawsMemoryAndBothSidesOfGrant(t *testing.T) {
	ctx := context.Background()
	c := clock.NewFake(time.Unix(1000, 0))
	d := lifecycle.NewMemDeletions()
	if _, err := d.MarkEndUser(ctx, &lifecycle.EndUserTombstone{BusinessLine: "bl", EndUser: "u", RequestID: "r", RequestedAt: c.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &memory.Service{Store: memory.NewMemStore(), Grants: memory.NewMemGrants(), Deletions: d, Clock: c, IDs: ids.Sequential("m")}
	if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "bl", EndUser: "u", Category: memory.Preference, Content: "偏好简洁回答"}); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("save = %v", err)
	}
	if rows, err := s.Store.List(ctx, "u", []memory.Scope{{BusinessLine: "bl"}}, memory.Everything, 100); err != nil || len(rows) != 0 {
		t.Fatalf("memories = %+v, %v", rows, err)
	}
	for _, pair := range [][2]string{{"bl", "other"}, {"other", "bl"}} {
		if _, err := s.CreateGrant(ctx, "u", pair[0], pair[1], []memory.Category{memory.Preference}, time.Time{}); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("grant %v = %v", pair, err)
		}
	}
	if grants, err := s.ListGrants(ctx, "u", "bl"); err != nil || len(grants) != 0 {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
}

// 两种保留原因都只保留既有 Memory；删除记录之后的新写入不能留下。
func TestDeletionWithdrawsLateMemory(t *testing.T) {
	for _, reason := range []string{lifecycle.ReasonRetention, lifecycle.ReasonErasure} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			c := clock.NewFake(time.Unix(1000, 0))
			d := lifecycle.NewMemDeletions()
			s := &memory.Service{Store: memory.NewMemStore(), Grants: memory.NewMemGrants(), Deletions: d, Clock: c, IDs: ids.Sequential("m")}
			before, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "bl", EndUser: "u", SessionID: "ses_v1_000_000_s", CallID: "before", Category: memory.Preference, Content: "喜欢骑车"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Mark(ctx, &lifecycle.Tombstone{SessionID: "ses_v1_000_000_s", BusinessLine: "bl", Reason: reason, RequestedAt: c.Now()}); err != nil {
				t.Fatal(err)
			}
			for _, complete := range []bool{false, true} {
				if complete {
					if err := d.Complete(ctx, "ses_v1_000_000_s", c.Now()); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "bl", EndUser: "u", SessionID: "ses_v1_000_000_s", CallID: "late", Category: memory.Preference, Content: "不吃香菜"}); !errors.Is(err, memory.ErrNotFound) {
					t.Fatalf("late save = %v", err)
				}
				if _, err := s.Store.Get(ctx, "u", scopedMemoryID("bl", "u", "ses_v1_000_000_s", "late")); !errors.Is(err, memory.ErrNotFound) {
					t.Fatalf("late memory survived: %v", err)
				}
				got, err := s.Store.ListBySource(ctx, "bl", "u", []string{"ses_v1_000_000_s"}, c.Now())
				if err != nil || len(got) != 1 || got[0].ID != before.ID {
					t.Fatalf("existing memory: %+v, %v", got, err)
				}
			}
		})
	}
}

type deletionDuringPut struct {
	memory.Store
	deletions lifecycle.Deletions
	reason    string
	at        time.Time
}

func (s deletionDuringPut) Put(ctx context.Context, m *memory.Memory, v *memory.Vector) error {
	// 模拟写入已经在途，删除先落盘、Memory 随后才落盘。
	if _, err := s.deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: m.SourceSession, BusinessLine: m.BusinessLine, Reason: s.reason, RequestedAt: s.at}); err != nil {
		return err
	}
	return s.Store.Put(ctx, m, v)
}

func TestInFlightMemoryWithdrawnOnErasure(t *testing.T) {
	ctx := context.Background()
	c := clock.NewFake(time.Unix(1000, 0))
	for _, reason := range []string{lifecycle.ReasonRetention, lifecycle.ReasonErasure} {
		t.Run(reason, func(t *testing.T) {
			d := lifecycle.NewMemDeletions()
			base := memory.NewMemStore()
			s := &memory.Service{Store: deletionDuringPut{Store: base, deletions: d, reason: reason, at: c.Now()}, Deletions: d, Clock: c, IDs: ids.Sequential("m")}
			_, err := s.Save(ctx, memory.SaveRequest{BusinessLine: "bl", EndUser: "u", SessionID: "ses_v1_000_000_s", CallID: "race", Category: memory.Preference, Content: "喜欢骑车"})
			if !errors.Is(err, memory.ErrNotFound) {
				t.Fatalf("save = %v", err)
			}
			if base.HasSession("ses_v1_000_000_s") {
				t.Fatal("in-flight memory survived deletion")
			}
		})
	}
}
