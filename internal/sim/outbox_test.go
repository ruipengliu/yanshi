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
