// Package pglog 是 eventlog.Log 的 PostgreSQL 实现。
//
// 乐观并发由主键 (session_id, seq) 保证：并发追加同一 seq 时只有一个事务成功，
// 其余因唯一约束冲突得到 ErrConflict。
package pglog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/pg"
)

type Log struct {
	pool     *pgxpool.Pool
	notifier *pg.Notifier
}

func New(pool *pgxpool.Pool, n *pg.Notifier) *Log { return &Log{pool: pool, notifier: n} }

var _ eventlog.Log = (*Log)(nil)

func (l *Log) Append(ctx context.Context, sessionID string, expectedSeq uint64, events ...*v1.Event) (uint64, error) {
	if len(events) == 0 {
		return expectedSeq, nil
	}
	rows := make([][]byte, len(events))
	for i, e := range events {
		e.SessionId = sessionID
		e.Seq = expectedSeq + uint64(i) + 1
		b, err := proto.Marshal(e)
		if err != nil {
			return 0, err
		}
		rows[i] = b
	}
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		var head int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM events WHERE session_id = $1`, sessionID).Scan(&head); err != nil {
			return err
		}
		if uint64(head) != expectedSeq {
			return eventlog.ErrConflict
		}
		batch := &pgx.Batch{}
		for i, b := range rows {
			batch.Queue(`INSERT INTO events (session_id, seq, data) VALUES ($1, $2, $3)`, sessionID, int64(expectedSeq)+int64(i)+1, b)
		}
		batch.Queue(`SELECT pg_notify($1, $2)`, pg.ChannelEvents, sessionID)
		return tx.SendBatch(ctx, batch).Close()
	})
	if pg.IsUniqueViolation(err) {
		return 0, eventlog.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	return expectedSeq + uint64(len(events)), nil
}

func (l *Log) Read(ctx context.Context, sessionID string, after uint64, limit int) ([]*v1.Event, error) {
	q := `SELECT data FROM events WHERE session_id = $1 AND seq > $2 ORDER BY seq`
	args := []any{sessionID, int64(after)}
	if limit > 0 {
		q += ` LIMIT $3`
		args = append(args, limit)
	}
	rows, err := l.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.Event, error) {
		var b []byte
		if err := r.Scan(&b); err != nil {
			return nil, err
		}
		e := &v1.Event{}
		if err := proto.Unmarshal(b, e); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		return e, nil
	})
	return out, err
}

func (l *Log) head(ctx context.Context, sessionID string) (uint64, error) {
	var head int64
	err := l.pool.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM events WHERE session_id = $1`, sessionID).Scan(&head)
	return uint64(head), err
}

func (l *Log) Wait(ctx context.Context, sessionID string, after uint64) (uint64, error) {
	var head uint64
	err := l.notifier.WaitFor(ctx, pg.ChannelEvents, sessionID, func(ctx context.Context) (bool, error) {
		h, err := l.head(ctx, sessionID)
		head = h
		return h > after, err
	})
	return head, err
}
