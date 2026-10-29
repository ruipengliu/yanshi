package service_test

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
)

func TestScheduleFireUpdatesIndex(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "fired"
		if closed {
			name = "skipped closed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, clk, sid := scheduleService(t, "UTC")
			s.Index = lifecycle.NewMemIndex()
			if err := s.Index.Put(ctx, &lifecycle.Session{ID: sid, BusinessLine: "bl", EndUser: "u", CreatedAt: clk.Now(), LastInputAt: clk.Now()}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateSchedule(ctx, sid, scheduleReq("s")); err != nil {
				t.Fatal(err)
			}
			if closed {
				if err := s.Close(ctx, sid, "u", "manual"); err != nil {
					t.Fatal(err)
				}
			}
			clk.Advance(2 * time.Hour)
			st, err := s.Load(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FireSchedule(ctx, st, "s"); err != nil {
				t.Fatal(err)
			}
			row, err := s.Index.Get(ctx, "u", sid)
			if err != nil || !row.NextDue.IsZero() {
				t.Fatalf("index %v %v", row, err)
			}
		})
	}
}
