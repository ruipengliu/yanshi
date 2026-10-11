// Package pgqueue 是 workqueue.Queue 的 PostgreSQL 实现。认领使用 FOR UPDATE SKIP LOCKED，
// 多个实例可并发认领而互不阻塞。时间由 clock.Clock 提供，便于测试与模拟。
package pgqueue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/clock"
	"yanshi/internal/pg"
	"yanshi/internal/workqueue"
)

// 队列表：Sessions 是 Worker 的工作队列，Sandboxes 是沙箱控制器的队列。
const (
	Sessions  = "work_items"
	Sandboxes = "sandbox_items"
	// Janitor 是 Session 清理（关闭与删除）的队列，键为 Session ID。
	Janitor = "janitor_items"
)

type Queue struct {
	pool  *pgxpool.Pool
	clock clock.Clock
	table string

	notifier  *pg.Notifier
	readyOnce sync.Once
	ready     <-chan struct{}
}

// WithNotifier 启用入队信号（workqueue.Signaler）：入队时 NOTIFY，各进程的空闲 Worker 立即认领。
func (q *Queue) WithNotifier(n *pg.Notifier) *Queue {
	q.notifier = n
	return q
}

// Ready 返回入队信号；未启用时返回 nil（永不就绪，等待者只靠轮询）。
func (q *Queue) Ready() <-chan struct{} {
	if q.notifier == nil {
		return nil
	}
	q.readyOnce.Do(func() { q.ready, _ = q.notifier.Subscribe(pg.TopicWork, q.table) })
	return q.ready
}

// New 返回基于 table（Sessions 或 Sandboxes）的队列。
func New(pool *pgxpool.Pool, c clock.Clock, table string) *Queue {
	if table != Sessions && table != Sandboxes && table != Janitor {
		panic("pgqueue: unknown table " + table)
	}
	return &Queue{pool: pool, clock: c, table: table}
}

// q 把 SQL 中的 work_items 替换为本队列的表名。
func (q *Queue) q(sql string) string { return strings.ReplaceAll(sql, "work_items", q.table) }

var (
	_ workqueue.Queue    = (*Queue)(nil)
	_ workqueue.Signaler = (*Queue)(nil)
)

func (q *Queue) Enqueue(ctx context.Context, sessionID string) error {
	// 同一语句内入队并通知，不增加往返；通知在提交后送达。
	_, err := q.pool.Exec(ctx, q.q(`
		WITH up AS (
			INSERT INTO work_items (session_id, ord) VALUES ($1, nextval('work_seq'))
			ON CONFLICT (session_id) DO UPDATE
			SET dirty = work_items.dirty OR work_items.leased, parked_until = NULL
			RETURNING 1)
		SELECT pg_notify($2, $3) FROM up`), sessionID, q.notifier.Channel(pg.TopicWork, q.table), q.table)
	return err
}

func (q *Queue) Claim(ctx context.Context, holder string, ttl time.Duration) (*workqueue.Lease, error) {
	now := q.clock.Now()
	l := &workqueue.Lease{Holder: holder}
	err := q.pool.QueryRow(ctx, q.q(`
		UPDATE work_items SET leased = true, holder = $1, token = nextval('work_seq'),
			lease_expires = $3, dirty = false
		WHERE session_id = (
			SELECT session_id FROM work_items
			WHERE (leased AND lease_expires <= $2)
			   OR (NOT leased AND (parked_until IS NULL OR parked_until <= $2))
			ORDER BY ord LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING session_id, token, lease_expires`), holder, now, now.Add(ttl)).Scan(&l.SessionID, &l.Token, &l.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workqueue.ErrEmpty
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// held 是"租约仍有效"的条件，参数 $1 session_id、$2 token、$3 now。
const held = `session_id = $1 AND leased AND token = $2 AND lease_expires > $3`

func (q *Queue) Renew(ctx context.Context, l *workqueue.Lease, ttl time.Duration) error {
	now := q.clock.Now()
	tag, err := q.pool.Exec(ctx, q.q(`UPDATE work_items SET lease_expires = $4 WHERE `+held),
		l.SessionID, int64(l.Token), now, now.Add(ttl))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return workqueue.ErrLeaseLost
	}
	l.Expires = now.Add(ttl)
	return nil
}

func (q *Queue) Park(ctx context.Context, l *workqueue.Lease, until time.Time) error {
	tag, err := q.pool.Exec(ctx, q.q(`
		UPDATE work_items SET leased = false, ord = nextval('work_seq'),
			parked_until = CASE WHEN dirty THEN NULL ELSE $4::timestamptz END, dirty = false
		WHERE `+held), l.SessionID, int64(l.Token), q.clock.Now(), until)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return workqueue.ErrLeaseLost
	}
	return nil
}

func (q *Queue) Release(ctx context.Context, l *workqueue.Lease, done bool) error {
	now := q.clock.Now()
	if done {
		tag, err := q.pool.Exec(ctx, q.q(`DELETE FROM work_items WHERE `+held+` AND NOT dirty`), l.SessionID, int64(l.Token), now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
	}
	tag, err := q.pool.Exec(ctx, q.q(`
		UPDATE work_items SET leased = false, dirty = false, parked_until = NULL, ord = nextval('work_seq')
		WHERE `+held), l.SessionID, int64(l.Token), now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return workqueue.ErrLeaseLost
	}
	return nil
}

func (q *Queue) Remove(ctx context.Context, sessionID string) error {
	_, err := q.pool.Exec(ctx, q.q(`DELETE FROM work_items WHERE session_id = $1`), sessionID)
	return err
}
