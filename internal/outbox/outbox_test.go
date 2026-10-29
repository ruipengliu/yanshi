package outbox_test

import (
	"context"
	"testing"

	"yanshi/internal/artifact"
	"yanshi/internal/sandbox"

	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/outbox/outboxtest"
	"yanshi/internal/pg/pgtest"
)

func stores(s app.Stores) outboxtest.Stores {
	return outboxtest.Stores{Log: s.Log, Waking: s.Waking, Queue: s.Queue, Deletions: s.Deletions, Outbox: s.Outbox}
}

func TestMemoryConformance(t *testing.T) {
	outboxtest.Run(t, func(t *testing.T) outboxtest.Stores { return stores(app.Memory(clock.Real{})) })
}

func TestPostgresConformance(t *testing.T) {
	outboxtest.Run(t, func(t *testing.T) outboxtest.Stores {
		pool, notifier := pgtest.Fresh(t)
		return stores(app.Postgres(pool, notifier, clock.Real{}))
	})
}

func TestSessionDataRegistersOutbox(t *testing.T) {
	s := app.Memory(clock.Real{})
	for _, holder := range app.SessionData(s, sandbox.Workspaces{}, &artifact.Service{}) {
		if holder.Name == "outbox" {
			if holder.KeepOnRetention {
				t.Fatal("ordinary envelopes must not survive retention or erasure")
			}
			if err := holder.DeleteSession(context.Background(), "s"); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("outbox missing from SessionData")
}

func TestMemoryStreamConformance(t *testing.T) {
	outboxtest.RunStream(t, func(t *testing.T) outboxtest.Stores { return stores(app.Memory(clock.Real{})) })
}

func TestPostgresStreamConformance(t *testing.T) {
	outboxtest.RunStream(t, func(t *testing.T) outboxtest.Stores {
		pool, notifier := pgtest.Fresh(t)
		return stores(app.Postgres(pool, notifier, clock.Real{}))
	})
}
