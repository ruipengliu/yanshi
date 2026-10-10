// Package pgpresence 是 presence.Store 的 PostgreSQL 实现：多进程共享在场记录，
// 变化时以 NOTIFY 唤醒各进程的等待者（变化频率按用户操作计，远低于 token 增量，ADR-0013）。
package pgpresence

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/pg"
	"yanshi/internal/presence"
)

// pollEvery 是兜底轮询间隔：通知是主要途径，在场对延迟不敏感，而等待者与订阅数一样多。
const pollEvery = 15 * time.Second

type Store struct {
	Pool     *pgxpool.Pool
	Notifier *pg.Notifier
}

var _ presence.Store = Store{}

// bump 在同一事务内递增版本并通知。
func bump(ctx context.Context, tx pgx.Tx, sid string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO presence_versions (session_id, version) VALUES ($1, 1)
		ON CONFLICT (session_id) DO UPDATE SET version = presence_versions.version + 1`, sid); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, pg.ChannelPresence, sid)
	return err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (s Store) Put(ctx context.Context, e *presence.Entry) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// 写入前读出旧行（加锁），比较显示内容决定是否递增版本。
		var old presence.Entry
		var typing *time.Time
		err := tx.QueryRow(ctx, `
			SELECT device_id, label, kind, focused, typing_until FROM presence
			WHERE session_id = $1 AND conn_id = $2 FOR UPDATE`, e.SessionID, e.ConnID).
			Scan(&old.DeviceID, &old.Label, &old.Kind, &old.Focused, &typing)
		existed := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if typing != nil {
			old.TypingUntil = *typing
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO presence (session_id, conn_id, device_id, label, kind, focused, typing_until, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (session_id, conn_id) DO UPDATE SET device_id = $3, label = $4, kind = $5, focused = $6,
				typing_until = $7, expires_at = $8`,
			e.SessionID, e.ConnID, e.DeviceID, e.Label, e.Kind, e.Focused, nullTime(e.TypingUntil), e.Expires); err != nil {
			return err
		}
		if existed && sameVisible(&old, e) {
			return nil
		}
		return bump(ctx, tx, e.SessionID)
	})
}

func sameVisible(a, b *presence.Entry) bool {
	// 数据库时间精度为微秒，比较前对齐。
	return a.DeviceID == b.DeviceID && a.Label == b.Label && a.Kind == b.Kind && a.Focused == b.Focused &&
		a.TypingUntil.Truncate(time.Microsecond).Equal(b.TypingUntil.Truncate(time.Microsecond))
}

func (s Store) Touch(ctx context.Context, connID string, expires time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE presence SET expires_at = $2 WHERE conn_id = $1`, connID, expires)
	return err
}

func (s Store) Remove(ctx context.Context, sessionID, connID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM presence WHERE session_id = $1 AND conn_id = $2`, sessionID, connID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return bump(ctx, tx, sessionID)
	})
}

func version(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sid string) (uint64, error) {
	var v int64
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT version FROM presence_versions WHERE session_id = $1), 0)`, sid).Scan(&v)
	return uint64(v), err
}

func (s Store) List(ctx context.Context, sessionID string, now time.Time) ([]*presence.Entry, uint64, error) {
	var out []*presence.Entry
	var v uint64
	// 可重复读事务保证记录与版本号来自同一快照。
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		if v, err = version(ctx, tx, sessionID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT conn_id, device_id, label, kind, focused, typing_until, expires_at FROM presence
			WHERE session_id = $1 AND expires_at > $2 ORDER BY conn_id`, sessionID, now)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*presence.Entry, error) {
			e := &presence.Entry{SessionID: sessionID}
			var typing *time.Time
			if err := r.Scan(&e.ConnID, &e.DeviceID, &e.Label, &e.Kind, &e.Focused, &typing, &e.Expires); err != nil {
				return nil, err
			}
			if typing != nil {
				e.TypingUntil = *typing
			}
			return e, nil
		})
		return err
	})
	return out, v, err
}

func (s Store) Wait(ctx context.Context, sessionID string, v uint64) error {
	return s.Notifier.WaitForEvery(ctx, pg.ChannelPresence, sessionID, pollEvery, func(ctx context.Context) (bool, error) {
		cur, err := version(ctx, s.Pool, sessionID)
		return cur != v, err
	})
}

func (s Store) DeleteSession(ctx context.Context, sessionID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM presence WHERE session_id = $1`, sessionID); err != nil {
			return err
		}
		// 先通知等待者，再删除版本号：Session 已删除，不会再有新的记录。
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, pg.ChannelPresence, sessionID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM presence_versions WHERE session_id = $1`, sessionID)
		return err
	})
}

func (s Store) Prune(ctx context.Context, before time.Time) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM presence WHERE expires_at <= $1`, before)
	return err
}

// HasSession 报告是否还有该 Session 的记录，供删除不变量检查。
func (s Store) HasSession(ctx context.Context, sessionID string) (bool, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM presence WHERE session_id = $1`, sessionID).Scan(&n)
	return n > 0, err
}
