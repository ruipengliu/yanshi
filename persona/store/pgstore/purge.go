package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"persona/pg"
	"persona/store"
)

// lockOwner 与 Update 共用锁协议。Purge 不删除锁行，等待者不会换到另一代 owner 锁。
func lockOwner(ctx context.Context, tx pgx.Tx, bl, eu []byte) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO owners (business_line,end_user) VALUES ($1,$2) ON CONFLICT DO NOTHING`, bl, eu)
	if err != nil {
		return false, err
	}
	var locked []byte
	if err := tx.QueryRow(ctx, `SELECT business_line FROM owners WHERE business_line=$1 AND end_user=$2 FOR UPDATE`, bl, eu).Scan(&locked); err != nil {
		return false, err
	}
	return tag.RowsAffected() != 0, nil
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func checkOwner(ctx context.Context, q querier, bl, eu []byte) error {
	var blocked bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM owner_deletions WHERE business_line=$1 AND end_user=$2)`, bl, eu).Scan(&blocked); err != nil {
		return contextError(ctx, err)
	}
	if blocked {
		return store.ErrOwnerDeleting
	}
	return nil
}

func readDeletion(ctx context.Context, q querier, bl, eu []byte) (store.OwnerDeletion, error) {
	out := store.OwnerDeletion{Owner: store.Owner{BusinessLine: string(bl), EndUser: string(eu)}}
	var completed *time.Time
	err := q.QueryRow(ctx, `SELECT status,started_at,completed_at FROM owner_deletions WHERE business_line=$1 AND end_user=$2`, bl, eu).Scan(&out.Status, &out.StartedAt, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OwnerDeletion{}, store.ErrDeletionNotStarted
	}
	if completed != nil {
		out.CompletedAt = *completed
	}
	return out, contextError(ctx, err)
}

func (s *Store) OwnerDeletion(ctx context.Context, bl, eu string) (store.OwnerDeletion, error) {
	return readDeletion(ctx, s.pool, []byte(bl), []byte(eu))
}

func (s *Store) BeginOwnerDeletion(ctx context.Context, businessLine, endUser string, now time.Time) (store.OwnerDeletion, error) {
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	if now.IsZero() {
		return store.OwnerDeletion{}, store.ErrDeletionTime
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return store.OwnerDeletion{}, contextError(ctx, err)
	}
	defer pg.Rollback(tx)
	bl, eu := []byte(businessLine), []byte(endUser)
	if _, err := lockOwner(ctx, tx, bl, eu); err != nil {
		return store.OwnerDeletion{}, contextError(ctx, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO owner_deletions (business_line,end_user,status,started_at) VALUES ($1,$2,'pending',$3) ON CONFLICT DO NOTHING`, bl, eu, now); err != nil {
		return store.OwnerDeletion{}, contextError(ctx, err)
	}
	out, err := readDeletion(ctx, tx, bl, eu)
	if err != nil {
		return store.OwnerDeletion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.OwnerDeletion{}, contextError(ctx, err)
	}
	return out, nil
}

func (s *Store) PendingPurges(ctx context.Context) ([]store.Owner, error) {
	rows, err := s.pool.Query(ctx, `SELECT business_line,end_user FROM owner_deletions WHERE status='pending' ORDER BY business_line,end_user`)
	if err != nil {
		return nil, contextError(ctx, err)
	}
	defer rows.Close()
	out := []store.Owner{}
	for rows.Next() {
		var bl, eu []byte
		if err := rows.Scan(&bl, &eu); err != nil {
			return nil, err
		}
		out = append(out, store.Owner{BusinessLine: string(bl), EndUser: string(eu)})
	}
	return out, contextError(ctx, rows.Err())
}

func (s *Store) Purge(ctx context.Context, businessLine, endUser string, limit int, now time.Time) (store.OwnerDeletion, error) {
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	if limit < 1 || limit > store.MaxPurgeBatch {
		return store.OwnerDeletion{}, store.ErrPurgeLimit
	}
	if now.IsZero() {
		return store.OwnerDeletion{}, store.ErrDeletionTime
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return store.OwnerDeletion{}, contextError(ctx, err)
	}
	defer pg.Rollback(tx)
	bl, eu := []byte(businessLine), []byte(endUser)
	if _, err := lockOwner(ctx, tx, bl, eu); err != nil {
		return store.OwnerDeletion{}, contextError(ctx, err)
	}
	out, err := readDeletion(ctx, tx, bl, eu)
	if err != nil {
		return store.OwnerDeletion{}, err
	}
	if out.Status == "completed" {
		return out, nil
	}
	// 不装载 Book，不触发整 owner 的级联。静态表目录复用账本持久化映射，新增类别自动纳入。
	tables := []string{"capture_segments", "capture_pending"}
	for _, c := range categories {
		tables = append(tables, c.table)
	}
	remaining := limit
	for _, table := range tables {
		// ctid 仅在本语句内定位，候选按 owner 主键前缀限量选出；表间没有级联。
		tag, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE business_line=$1 AND end_user=$2 AND ctid IN (SELECT ctid FROM `+table+` WHERE business_line=$1 AND end_user=$2 LIMIT $3)`, bl, eu, remaining)
		if err != nil {
			return store.OwnerDeletion{}, contextError(ctx, err)
		}
		remaining -= int(tag.RowsAffected())
		if remaining == 0 {
			break
		}
	}
	// 恰好用完预算时留待下一批确认空库，不能把“本批没看见的表”误判为完成。
	if remaining > 0 {
		out.Status, out.CompletedAt = "completed", now.UTC()
		if out.CompletedAt.Before(out.StartedAt) {
			out.CompletedAt = out.StartedAt
		}
		if _, err := tx.Exec(ctx, `UPDATE owner_deletions SET status='completed',completed_at=$3 WHERE business_line=$1 AND end_user=$2`, bl, eu, out.CompletedAt); err != nil {
			return store.OwnerDeletion{}, contextError(ctx, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.OwnerDeletion{}, contextError(ctx, err)
	}
	return out, nil
}
