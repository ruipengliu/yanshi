package pgoutbox_test

import (
	"context"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/outbox"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/pgqueue"
)

// xid8 必须来自实际提交事务；同批多行共用 xid，行号唯一，为 B4 留下可靠的排序依据。
func TestTransactionMetadata(t *testing.T) {
	pool, notifier := pgtest.Fresh(t)
	ctx := context.Background()
	log := pglog.New(pool, notifier)
	queue := pgqueue.New(pool, clock.Real{}, pgqueue.Sessions).WithNotifier(notifier)
	for _, wake := range []bool{false, true} {
		sid := "ses_v1_000_000_plain"
		if wake {
			sid = "ses_v1_000_000_waking"
		}
		var envelopes []outbox.Envelope
		var events []*v1.Event
		for seq := uint64(1); seq <= 2; seq++ {
			envelopes = append(envelopes, outbox.Envelope{EnvelopeID: outbox.ID(sid, seq, outbox.RunCompleted), BusinessLine: "bl", SessionID: sid, Seq: seq, Kind: outbox.RunCompleted})
			events = append(events, &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: "r"}}})
		}
		var err error
		if wake {
			_, err = log.Waking(queue).AppendAndEnqueue(ctx, sid, 0, workqueue.Class{BusinessLine: "bl"}, envelopes, events...)
		} else {
			_, err = log.Append(ctx, sid, 0, envelopes, events...)
		}
		if err != nil {
			t.Fatal(err)
		}
		var count, transactions, ids int
		if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT xid), count(DISTINCT id) FROM outbox WHERE session_id=$1`, sid).Scan(&count, &transactions, &ids); err != nil {
			t.Fatal(err)
		}
		if count != 2 || transactions != 1 || ids != 2 {
			t.Fatalf("rows=%d transactions=%d ids=%d", count, transactions, ids)
		}
		var same bool
		if err := pool.QueryRow(ctx, `SELECT bool_and((o.xid::text::numeric % 4294967296)::text = e.xmin::text) FROM outbox o JOIN events e USING (session_id, seq) WHERE o.session_id=$1`, sid).Scan(&same); err != nil {
			t.Fatal(err)
		}
		if !same {
			t.Fatal("event and envelope used different transactions")
		}
	}
	d := pglifecycle.Deletions{Pool: pool, Notifier: notifier}
	if _, err := d.Mark(ctx, &lifecycle.Tombstone{SessionID: "ses_v1_000_000_deleted", BusinessLine: "bl", Reason: "service", RequestedAt: clock.Real{}.Now()}); err != nil {
		t.Fatal(err)
	}
	var same bool
	if err := pool.QueryRow(ctx, `SELECT (o.xid::text::numeric % 4294967296)::text = t.xmin::text AND o.seq IS NULL AND o.end_user IS NULL
		FROM outbox o JOIN session_tombstones t USING (session_id) WHERE o.session_id='ses_v1_000_000_deleted'`).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("deletion envelope is not in Mark transaction or contains event identity")
	}
}
