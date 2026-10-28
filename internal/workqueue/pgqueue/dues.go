package pgqueue

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"yanshi/internal/workqueue"
)

var _ workqueue.Deferrer = (*Queue)(nil)

// WithHorizon 在装配时设置远近停放的边界，须大于 Sweep 周期的若干倍。
func (q *Queue) WithHorizon(h time.Duration) *Queue {
	if h <= 0 {
		panic("pgqueue: non-positive horizon")
	}
	q.horizon = h
	return q
}

func (q *Queue) Horizon() time.Duration { return q.horizon }

// begin 在读取之前取得事务锁；锁语句和后续读取分开，等待者才能看见前任提交的新行。
// 行锁无法保护两表都暂时没有行的键。哈希碰撞至多串行化无关的键，不影响正确性。
func (q *Queue) begin(ctx context.Context, sid string) (pgx.Tx, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if q.table == Sessions {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "work_dues:"+sid); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
	}
	return tx, nil
}

// BeginEnqueue 为独立入队与日志事务内入队共用同一搬移边界；调用者负责提交或回滚。
// 先恢复停放项，随后 UpsertSQL 按原语义唤醒并保留 ord；日志冲突时整个恢复也回滚。
func (q *Queue) BeginEnqueue(ctx context.Context, sid string) (pgx.Tx, error) {
	tx, err := q.begin(ctx, sid)
	if err != nil {
		return nil, err
	}
	if q.table == Sessions {
		_, err = tx.Exec(ctx, `WITH moved AS (
			DELETE FROM work_dues WHERE session_id = $1 RETURNING *)
			INSERT INTO work_items (session_id, business_line, priority, ord, parked_until, ready_at)
			SELECT session_id, business_line, priority, ord, due_at, due_at FROM moved`, sid)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
	}
	return tx, nil
}

type due struct {
	sid string
	ord int64
}

// Promote 的候选扫描只读到期索引；每个候选在自己的事务里重查 ord 与期限。
// ord 每次 Park 都变化，候选过时后不能搬走同一 Session 后来重新停放的项。
func (q *Queue) Promote(ctx context.Context, before time.Time, limit int) (int, error) {
	if q.table != Sessions || limit <= 0 {
		return 0, nil
	}
	rows, err := q.pool.Query(ctx, `SELECT session_id, ord FROM work_dues
		WHERE due_at <= $1 ORDER BY due_at, session_id LIMIT $2`, before, limit)
	if err != nil {
		return 0, err
	}
	candidates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		err := r.Scan(&d.sid, &d.ord)
		return d, err
	})
	if err != nil {
		return 0, err
	}
	// 有界并发摊薄逐 Session 提交的往返，不能用跨键事务换吞吐。
	var mu sync.Mutex
	var wg sync.WaitGroup
	next, moved := 0, 0
	var firstErr error
	for range min(16, len(candidates)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next == len(candidates) || firstErr != nil {
					mu.Unlock()
					return
				}
				d := candidates[next]
				next++
				mu.Unlock()
				n, err := q.promote(ctx, d, before)
				mu.Lock()
				moved += n
				if err != nil && firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return moved, firstErr
}

func (q *Queue) promote(ctx context.Context, d due, before time.Time) (int, error) {
	tx, err := q.begin(ctx, d.sid)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `WITH moved AS (
		DELETE FROM work_dues WHERE session_id = $1 AND ord = $2 AND due_at <= $3 RETURNING *)
		INSERT INTO work_items (session_id, business_line, priority, ord, parked_until, ready_at)
		SELECT session_id, business_line, priority, ord, due_at, due_at FROM moved`, d.sid, d.ord, before)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Contains 把两张表作为一个队列观察；Depths 仍只统计可认领项。
func (q *Queue) Contains(ctx context.Context, sid string) (bool, error) {
	sql := q.q(`SELECT EXISTS (SELECT 1 FROM work_items WHERE session_id = $1)`)
	if q.table == Sessions {
		sql += ` OR EXISTS (SELECT 1 FROM work_dues WHERE session_id = $1)`
	}
	var has bool
	err := q.pool.QueryRow(ctx, sql, sid).Scan(&has)
	return has, err
}
