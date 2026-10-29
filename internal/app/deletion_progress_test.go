package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/service"
	"yanshi/internal/shard"
)

func TestDeletionProgressAuthorizesBeforeFanout(t *testing.T) {
	l := splitLocator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parts := map[int]app.Stores{0: app.MemoryShard(clock.Real{}, l), 1: app.MemoryShard(clock.Real{}, l)}
	if err := parts[0].Deletions.CreateRequest(ctx, &lifecycle.Request{ID: "delete", BusinessLine: "bl", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var contacted atomic.Int32
	s, err := app.Sharded(ctx, l, parts, app.ShardOptions{Available: func(_ context.Context, id int) error {
		if id == 1 {
			contacted.Add(1)
			return shard.ErrUnavailable
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{Deletions: s.Deletions}
	if _, err := svc.DeletionRequest(ctx, "other", "delete"); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("authorization: %v", err)
	}
	if contacted.Load() != 0 {
		t.Fatal("unauthorized query contacted data shard")
	}
	if result, err := svc.DeletionRequest(ctx, "bl", "delete"); !errors.Is(err, shard.ErrUnavailable) || result != nil {
		t.Fatalf("missing shard reported complete: %+v %v", result, err)
	}
}
