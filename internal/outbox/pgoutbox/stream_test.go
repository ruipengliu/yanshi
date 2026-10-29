package pgoutbox_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/clock"
	"yanshi/internal/outbox"
	"yanshi/internal/outbox/outboxtest"
	"yanshi/internal/outbox/pgoutbox"
	"yanshi/internal/pg/pgtest"
)

func beginOutbox(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	return tx
}

func insertOutbox(t *testing.T, tx pgx.Tx, sid string, seq uint64) outbox.Envelope {
	t.Helper()
	e := outbox.Envelope{EnvelopeID: outbox.ID(sid, seq, outbox.RunCompleted), BusinessLine: "bl", SessionID: sid, Seq: seq, Kind: outbox.RunCompleted}
	if err := pgoutbox.Insert(context.Background(), tx, []outbox.Envelope{e}); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSafeWatermarkOutOfOrderCommitRollbackAndSameXID(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	s := pgoutbox.Store{Pool: pool}
	ctx := context.Background()
	a := beginOutbox(t, pool)
	a1 := insertOutbox(t, a, "a", 1)
	b := beginOutbox(t, pool)
	b1 := insertOutbox(t, b, "b", 1)
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Stats(ctx)
	if err != nil || stats.Rows != 1 || stats.WatermarkLag == 0 || stats.Bytes <= 0 {
		t.Fatalf("blocked watermark stats=%+v %v", stats, err)
	}
	req := outbox.Request{BusinessLine: "bl", Limit: 1}
	blocked, err := s.Read(ctx, req)
	if err != nil || len(blocked.Events) != 0 || blocked.Next == "" {
		t.Fatalf("blocked read=%+v %v", blocked, err)
	}
	// A 在 B 之后才取第二个行号，排序仍是 A 的两行再 B，而不是按 bigserial。
	a2 := insertOutbox(t, a, "a", 2)
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rolled := beginOutbox(t, pool)
	insertOutbox(t, rolled, "rolled", 1)
	if err := rolled.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	outboxtest.Published(t, s, outbox.Request{BusinessLine: "bl"}, 3)
	req.After = blocked.Next
	for _, want := range []outbox.Envelope{a1, a2, b1} {
		page, err := s.Read(ctx, req)
		if err != nil || !reflect.DeepEqual(page.Events, []outbox.Envelope{want}) {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		req.After = page.Next
	}
	page, err := s.Read(ctx, req)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("rollback leaked=%+v %v", page, err)
	}
}

func TestLongTransactionRetentionStartsAtPublication(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	s := pgoutbox.Store{Pool: pool}
	ctx := context.Background()
	req := outbox.Request{BusinessLine: "bl"}
	blocker := beginOutbox(t, pool)
	if _, err := blocker.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	tx := beginOutbox(t, pool)
	insertOutbox(t, tx, "waiting", 1)
	deleted := outbox.Deleted("bl", "deleted", "service", "")
	if err := pgoutbox.Insert(ctx, tx, []outbox.Envelope{deleted}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	for _, at := range []time.Time{now, now.Add(96 * time.Hour)} {
		if err := s.Maintain(ctx, clock.NewFake(at), 100); err != nil {
			t.Fatal(err)
		}
		page, err := s.Read(ctx, req)
		if err != nil || len(page.Events) != 0 {
			t.Fatalf("blocked=%+v %v", page, err)
		}
		if rows, err := s.List(ctx, "bl"); err != nil || len(rows) != 2 {
			t.Fatal("unpublished rows were pruned")
		}
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	outboxtest.Published(t, s, req, 2)
	head, err := s.Head(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	first := now.Add(97 * time.Hour)
	if err := s.Maintain(ctx, clock.NewFake(first), 100); err != nil {
		t.Fatal(err)
	}
	if err := s.Maintain(ctx, clock.NewFake(first.Add(outbox.Retention-time.Second)), 100); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.List(ctx, "bl"); err != nil || len(rows) != 2 {
		t.Fatal("publication retention shorter than 72h")
	}
	if err := s.Maintain(ctx, clock.NewFake(first.Add(outbox.Retention)), 1); err != nil {
		t.Fatal(err)
	}
	var floor, target string
	if err := pool.QueryRow(ctx, `SELECT floor::text,prune_target::text FROM outbox_stream`).Scan(&floor, &target); err != nil {
		t.Fatal(err)
	}
	if floor == target || target == "0" {
		t.Fatal("floor advanced before batches completed")
	}
	if err := s.Maintain(ctx, clock.NewFake(first.Add(outbox.Retention)), 100); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.List(ctx, "bl"); err != nil || len(rows) != 0 {
		t.Fatal("ordinary or deletion envelope survived retention")
	}
	// Head 可能恰好等于清理地板，此时有效；不能把等于地板也当过期。
	_, err = s.Read(ctx, outbox.Request{BusinessLine: "bl", After: head})
	if err != nil && !errors.Is(err, outbox.ErrExpired) {
		t.Fatal(err)
	}
}

func TestCursorAndFloorSurviveStoreReconstruction(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	ctx := context.Background()
	s := pgoutbox.Store{Pool: pool}
	req := outbox.Request{BusinessLine: "bl"}
	old, err := s.Read(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	tx := beginOutbox(t, pool)
	insertOutbox(t, tx, "s", 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	outboxtest.Published(t, s, req, 1)
	rebuilt := pgoutbox.Store{Pool: pool}
	req.After = old.Next
	if page, err := rebuilt.Read(ctx, req); err != nil || len(page.Events) != 1 {
		t.Fatalf("cursor did not survive reconstruction: %+v %v", page, err)
	}
	now := clock.NewFake(time.Unix(1000, 0))
	if err := s.Maintain(ctx, now, 1); err != nil {
		t.Fatal(err)
	}
	now.Advance(outbox.Retention)
	if err := rebuilt.Maintain(ctx, now, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, req); !errors.Is(err, outbox.ErrExpired) {
		t.Fatalf("durable floor=%v", err)
	}
	if err := rebuilt.RotateEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, req); !errors.Is(err, outbox.ErrEpoch) {
		t.Fatalf("durable epoch=%v", err)
	}
}

func TestReadOrdersXIDNumerically(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	ctx := context.Background()
	tx := beginOutbox(t, pool)
	a, b := insertOutbox(t, tx, "a", 1), insertOutbox(t, tx, "b", 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 固定数字跨位数的边界，避免 xid::text 的输出列别名意外控制排序。
	if _, err := pool.Exec(ctx, `UPDATE outbox SET xid=CASE WHEN session_id='a' THEN '9'::xid8 ELSE '10'::xid8 END`); err != nil {
		t.Fatal(err)
	}
	s := pgoutbox.Store{Pool: pool}
	page := outboxtest.Published(t, s, outbox.Request{BusinessLine: "bl"}, 2)
	if !reflect.DeepEqual(page.Events, []outbox.Envelope{a, b}) {
		t.Fatalf("xid ordering=%+v", page.Events)
	}
}
