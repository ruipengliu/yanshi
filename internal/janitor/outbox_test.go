package janitor_test

import (
	"context"
	"errors"
	"testing"

	"yanshi/internal/outbox"
)

func TestSweepMaintainsOutboxPublicationRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	req := outbox.Request{BusinessLine: "bl"}
	old, err := e.stors.Outbox.Head(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	sid := e.session(t, "day")
	if err := e.core.Service.Close(ctx, sid, "service", "test"); err != nil {
		t.Fatal(err)
	}
	j := e.core.Janitor("j", nil, nil, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, err := e.stors.Outbox.List(ctx, "bl"); err != nil || len(rows) != 1 {
		t.Fatal("new envelope was pruned")
	}
	e.clk.Advance(outbox.Retention)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, err := e.stors.Outbox.List(ctx, "bl"); err != nil || len(rows) != 0 {
		t.Fatal("Janitor did not prune outbox")
	}
	req.After = old
	if _, err := e.stors.Outbox.Read(ctx, req); !errors.Is(err, outbox.ErrExpired) {
		t.Fatalf("floor=%v", err)
	}
}
