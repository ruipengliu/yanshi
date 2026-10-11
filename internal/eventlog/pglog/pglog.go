// Package pglog 是 eventlog.Log 的 PostgreSQL 实现。
//
// 乐观并发由主键 (session_id, seq) 保证：并发追加同一 seq 时只有一个事务成功，
// 其余因唯一约束冲突得到 ErrConflict。
package pglog

import (
	"context"
	"errors"
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
	// 序列化时临时填入 Session 与 seq，成功后才写回调用方的事件：冲突时调用方同步后以同一批事件重试，
	// 留下的旧 seq 会使重试校验失败（与 memlog 一致，eventlogtest 检查）。
	rows := make([][]byte, len(events))
	for i, e := range events {
		sid, seq := e.SessionId, e.Seq
		e.SessionId, e.Seq = sessionID, expectedSeq+uint64(i)+1
		b, err := proto.Marshal(e)
		e.SessionId, e.Seq = sid, seq
		if err != nil {
			return 0, err
		}
		rows[i] = b
	}
	// 一条语句、一次往返完成检查、插入与通知（原来是事务中的五条语句）：末尾不是 expectedSeq 时不插入任何行；
	// 并发的同 seq 追加由主键冲突裁决。只有插入成功才通知，负载带上新的末尾 seq，等待者无需再查询
	// （pg.Notifier.WaitAbove）。
	var n int
	err := l.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO events (session_id, seq, data)
			SELECT $1, $2 + t.ord, t.d FROM unnest($3::bytea[]) WITH ORDINALITY AS t(d, ord)
			WHERE (SELECT coalesce(max(seq), 0) FROM events WHERE session_id = $1) = $2
			RETURNING 1)
		SELECT count(*), pg_notify($4, $5) FROM ins HAVING count(*) > 0`,
		sessionID, int64(expectedSeq), rows, l.notifier.Channel(pg.TopicEvents, sessionID),
		fmt.Sprintf("%s:%d", sessionID, expectedSeq+uint64(len(events))),
	).Scan(&n, nil)
	if errors.Is(err, pgx.ErrNoRows) || pg.IsUniqueViolation(err) {
		return 0, eventlog.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	for i, e := range events {
		e.SessionId, e.Seq = sessionID, expectedSeq+uint64(i)+1
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
	return l.notifier.WaitAbove(ctx, pg.TopicEvents, sessionID, after, func(ctx context.Context) (uint64, error) {
		return l.head(ctx, sessionID)
	})
}

func (l *Log) Delete(ctx context.Context, sessionID string) error {
	_, err := l.pool.Exec(ctx, `DELETE FROM events WHERE session_id = $1`, sessionID)
	return err
}

// HeadHint 来自本进程收到的提交通知（pg.Notifier 的缓存）。
func (l *Log) HeadHint(sessionID string) (uint64, bool) {
	return l.notifier.Latest(pg.TopicEvents, sessionID)
}
