package app_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/shard"
)

func TestRetentionScanContinuesPastUnfinishedCandidate(t *testing.T) {
	l := splitLocator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := app.Sharded(ctx, l, map[int]app.Stores{0: app.MemoryShard(clock.Real{}, l), 1: app.MemoryShard(clock.Real{}, l)}, app.ShardOptions{Available: func(_ context.Context, id int) error {
		if id == 1 {
			return shard.ErrUnavailable
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 3 {
		eu := userIn(l, 0)
		sid, err := ids.SessionID(fmt.Sprintf("scan-%d", i), eu)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Index.Put(ctx, &lifecycle.Session{ID: sid, BusinessLine: "bl", EndUser: eu, LastInputAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 3 {
		rows, err := s.Index.IdleBefore(ctx, "bl", lifecycle.AgentFilter{}, now, 1)
		if err != nil || len(rows) != 1 || seen[rows[0]] {
			t.Fatalf("stuck at unfinished candidate: %v %v", rows, err)
		}
		seen[rows[0]] = true
	}
	rows, err := s.Index.IdleBefore(ctx, "bl", lifecycle.AgentFilter{}, now, 1)
	if err != nil || len(rows) != 0 {
		t.Fatalf("scan did not reach end: %v %v", rows, err)
	}
	rows, err = s.Index.IdleBefore(ctx, "bl", lifecycle.AgentFilter{}, now, 1)
	if err != nil || len(rows) != 1 || !seen[rows[0]] {
		t.Fatal("unfinished candidates not retried after wrap")
	}
}
