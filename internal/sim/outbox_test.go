package sim

import (
	"context"
	"testing"

	"yanshi/internal/outbox"
	"yanshi/internal/outbox/memoutbox"
)

func TestOutboxInvariantRejectsFailedCommitEnvelope(t *testing.T) {
	w, err := New(Options{Seed: 1, Sessions: 1, Workers: 1, Ticks: 1})
	if err != nil {
		t.Fatal(err)
	}
	sid := w.sessions[0]
	e := outbox.Envelope{EnvelopeID: outbox.ID(sid, 100, outbox.RunCompleted), BusinessLine: "bl", SessionID: sid, Seq: 100, Kind: outbox.RunCompleted}
	if err := w.stores.Outbox.(*memoutbox.Store).Commit(context.Background(), []outbox.Envelope{e}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := w.checkOutbox(); err == nil {
		t.Fatal("accepted envelope without committed event")
	}
}

type missingDeletion struct{ outbox.Store }

func (s missingDeletion) List(ctx context.Context, bl string) ([]outbox.Envelope, error) {
	rows, err := s.Store.List(ctx, bl)
	var kept []outbox.Envelope
	for _, e := range rows {
		if e.Kind != outbox.SessionDeleted {
			kept = append(kept, e)
		}
	}
	return kept, err
}

func TestOutboxInvariantChecksReplacedDeletedSession(t *testing.T) {
	w, err := New(Options{Seed: 1, Sessions: 1, Workers: 1, Ticks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.deleteSession(); err != nil {
		t.Fatal(err)
	}
	if err := w.checkOutbox(); err != nil {
		t.Fatal(err)
	}
	w.stores.Outbox = missingDeletion{w.stores.Outbox}
	if err := w.checkOutbox(); err == nil {
		t.Fatal("lost deletion envelope was not detected after session replacement")
	}
}

func TestOutboxConsumerCrashReplayAndExactlyOnce(t *testing.T) {
	w, err := New(Options{Seed: 1, Sessions: 1, Workers: 1, Ticks: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e := outbox.Deleted("bl", "consumer-only", "service", "")
	if err := w.stores.Outbox.(*memoutbox.Store).Commit(ctx, []outbox.Envelope{e}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	w.outboxConsumer.steps = 4
	if _, err := w.consumeOutbox(ctx, true); err != nil {
		t.Fatal(err)
	}
	if w.outboxConsumer.cursor != "" || len(w.outboxConsumer.received) != 0 {
		t.Fatal("pre-commit crash persisted a partial receipt")
	}
	if _, err := w.consumeOutbox(ctx, true); err != nil {
		t.Fatal(err)
	}
	if w.Stats.OutboxConsumerCrashes != 2 || w.Stats.OutboxRedeliveries != 1 {
		t.Fatal("crash or replay path was not exercised")
	}
	if err := w.drainOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.checkOutboxConsumer([]outbox.Envelope{e}); err != nil {
		t.Fatal(err)
	}
	w.outboxConsumer.effects[e.EnvelopeID]++
	if err := w.checkOutboxConsumer([]outbox.Envelope{e}); err == nil {
		t.Fatal("duplicate effect accepted")
	}
	w.outboxConsumer.effects[e.EnvelopeID] = 1
	delete(w.outboxConsumer.received, e.EnvelopeID)
	if err := w.checkOutboxConsumer([]outbox.Envelope{e}); err == nil {
		t.Fatal("missing receipt accepted")
	}
}

func TestOutboxRetentionConsumerAndLogInvariant(t *testing.T) {
	w, err := New(Options{Seed: 1, Sessions: 1, Workers: 1, Ticks: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.svc.Close(ctx, w.sessions[0], "sim", "test"); err != nil {
		t.Fatal(err)
	}
	maintainer := w.core.Stores.Outbox
	if err := maintainer.Maintain(ctx, w.clock, 1); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(outbox.Retention)
	for range 3 {
		if err := maintainer.Maintain(ctx, w.clock, 1); err != nil {
			t.Fatal(err)
		}
	}
	if w.Stats.OutboxPruned == 0 {
		t.Fatal("retention was not exercised")
	}
	if err := w.checkOutbox(); err != nil {
		t.Fatal(err)
	}
	if err := w.drainOutbox(ctx); err != nil {
		t.Fatal(err)
	}
}
