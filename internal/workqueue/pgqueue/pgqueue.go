// Package pgqueue 是 workqueue.Queue 的 PostgreSQL 实现。认领使用 FOR UPDATE SKIP LOCKED，
// 多个实例可并发认领而互不阻塞。时间由 clock.Clock 提供，便于测试与模拟。
package pgqueue

import (
	"context"
	"errors"
	"fmt"
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
	_ workqueue.Stats    = (*Queue)(nil)
)

func (q *Queue) Enqueue(ctx context.Context, sessionID string, class workqueue.Class) error {
	// 同一语句内入队并通知，不增加往返；通知在提交后送达。
	_, err := q.pool.Exec(ctx, `WITH up AS (`+q.UpsertSQL("SELECT $1, $4::text, $5::smallint, $6::timestamptz")+`)
		SELECT pg_notify($2, $3) FROM up`, sessionID, q.notifier.Channel(pg.TopicWork, q.table), q.table,
		class.BusinessLine, int16(class.Priority), q.clock.Now())
	return err
}

// Now 是队列的当前时间（入队时刻，UpsertSQL 的 ready_at）。
func (q *Queue) Now() time.Time { return q.clock.Now() }

// UpsertSQL 是入队的数据修改语句（RETURNING 1），供其他写入在同一语句中入队（pglog 的 eventlog.WakingLog）。
// rows 是产生 (session_id, business_line, priority, 当前时刻) 的查询：它没有行时不入队。
// 已租出的项在归还时才变为可认领，挂起中的项被唤醒时从现在起算等待，已可认领的项保持原来的时刻。
func (q *Queue) UpsertSQL(rows string) string {
	return q.q(`
		INSERT INTO work_items (session_id, business_line, priority, ready_at, ord)
		SELECT r.*, nextval('work_seq') FROM (` + rows + `) AS r
		ON CONFLICT (session_id) DO UPDATE
		SET dirty = work_items.dirty OR work_items.leased, parked_until = NULL,
			business_line = EXCLUDED.business_line, priority = EXCLUDED.priority,
			ready_at = CASE WHEN NOT work_items.leased AND work_items.parked_until > EXCLUDED.ready_at
				THEN EXCLUDED.ready_at ELSE work_items.ready_at END
		RETURNING 1`)
}

// NotifyArgs 是入队通知的频道与负载。
func (q *Queue) NotifyArgs(sessionID string) (channel, payload string) {
	return q.notifier.Channel(pg.TopicWork, q.table), q.table
}

func (q *Queue) Claim(ctx context.Context, holder string, ttl time.Duration, p workqueue.Pool) (*workqueue.Lease, error) {
	now := q.clock.Now()
	l := &workqueue.Lease{Holder: holder}
	var prio int16
	var since time.Time
	// 索引 (priority DESC, ord) 使认领按优先级、再按入队顺序扫描。
	err := q.pool.QueryRow(ctx, q.q(`
		WITH pick AS (
			SELECT session_id, CASE WHEN leased THEN lease_expires ELSE ready_at END AS since FROM work_items
			WHERE ((leased AND lease_expires <= $2)
			    OR (NOT leased AND (parked_until IS NULL OR parked_until <= $2)))
			  AND priority >= $4
			  AND (coalesce(array_length($5::text[], 1), 0) = 0 OR business_line = ANY ($5::text[]))
			ORDER BY priority DESC, ord LIMIT 1
			FOR UPDATE SKIP LOCKED)
		UPDATE work_items w SET leased = true, holder = $1, token = nextval('work_seq'),
			lease_expires = $3, dirty = false
		FROM pick WHERE w.session_id = pick.session_id
		RETURNING w.session_id, w.token, w.lease_expires, w.business_line, w.priority, pick.since`),
		holder, now, now.Add(ttl), int16(p.MinPriority), p.BusinessLines).Scan(&l.SessionID, &l.Token, &l.Expires, &l.Class.BusinessLine, &prio, &since)
	l.Waited = max(now.Sub(since), 0)
	l.Class.Priority = workqueue.Priority(prio)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workqueue.ErrEmpty
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// returnedPriority 是归还时的优先级，规则同 workqueue.ReturnedClass；bl 与 prio 是持有者给的业务线与优先级的参数。
// SET 中的列引用读的是更新前的值。
func returnedPriority(bl, prio string) string {
	return fmt.Sprintf(`CASE WHEN dirty AND priority = %d AND %s::smallint = %d AND business_line = %s::text THEN priority ELSE %s::smallint END`,
		workqueue.Resumed, prio, workqueue.Background, bl, prio)
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
			parked_until = CASE WHEN dirty THEN NULL ELSE $4::timestamptz END,
			ready_at = CASE WHEN dirty THEN $3 ELSE $4::timestamptz END, dirty = false,
			business_line = $5, priority = `+returnedPriority("$5", "$6")+`
		WHERE `+held), l.SessionID, int64(l.Token), q.clock.Now(), until, l.Class.BusinessLine, int16(l.Class.Priority))
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
		UPDATE work_items SET leased = false, dirty = false, parked_until = NULL, ord = nextval('work_seq'),
			business_line = $4, priority = `+returnedPriority("$4", "$5")+`, ready_at = $3
		WHERE `+held), l.SessionID, int64(l.Token), now, l.Class.BusinessLine, int16(l.Class.Priority))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return workqueue.ErrLeaseLost
	}
	return nil
}

func (q *Queue) Depths(ctx context.Context) ([]workqueue.Depth, error) {
	now := q.clock.Now()
	rows, err := q.pool.Query(ctx, q.q(`
		SELECT business_line, priority, count(*), min(CASE WHEN leased THEN lease_expires ELSE ready_at END)
		FROM work_items
		WHERE (leased AND lease_expires <= $1) OR (NOT leased AND (parked_until IS NULL OR parked_until <= $1))
		GROUP BY business_line, priority`), now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (workqueue.Depth, error) {
		var d workqueue.Depth
		var prio int16
		var oldest time.Time
		err := r.Scan(&d.Class.BusinessLine, &prio, &d.Ready, &oldest)
		d.Class.Priority, d.OldestWait = workqueue.Priority(prio), max(now.Sub(oldest), 0)
		return d, err
	})
}

func (q *Queue) Remove(ctx context.Context, sessionID string) error {
	_, err := q.pool.Exec(ctx, q.q(`DELETE FROM work_items WHERE session_id = $1`), sessionID)
	return err
}
