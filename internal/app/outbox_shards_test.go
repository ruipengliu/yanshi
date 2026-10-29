package app_test

import (
	"context"
	"testing"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/outbox/outboxtest"
	"yanshi/internal/pg/pgtest"
)

func TestOutboxShards(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			l := splitLocator(t)
			parts := map[int]app.Stores{}
			for id := range 2 {
				parts[id] = app.MemoryShard(clock.Real{}, l)
				if backend == "postgres" {
					pool, notifier := pgtest.FreshDatabase(t)
					parts[id] = app.PostgresShard(pool, notifier, clock.Real{}, l, id)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			s, err := app.Sharded(ctx, l, parts, app.ShardOptions{})
			if err != nil {
				t.Fatal(err)
			}
			outboxtest.RunShards(t, outboxtest.Stores{Log: s.Log, Waking: s.Waking, Queue: s.Queue, Deletions: s.Deletions, Outbox: s.Outbox}, [2]string{keysIn(t, l, 0, "u"), keysIn(t, l, 1, "u")})
		})
	}
}
