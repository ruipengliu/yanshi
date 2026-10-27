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
