package pglifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"yanshi/internal/lifecycle"
)

const endUserColumns = `business_line, end_user, request_id, requested_at, sealed, session_ids, revision, completed_at, expires_at`

func scanEndUser(row pgx.Row) (*lifecycle.EndUserTombstone, error) {
	b := &lifecycle.EndUserTombstone{}
	var done, expires *time.Time
	err := row.Scan(&b.BusinessLine, &b.EndUser, &b.RequestID, &b.RequestedAt, &b.Sealed, &b.Sessions, &b.Revision, &done, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, lifecycle.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.RequestedAt = b.RequestedAt.UTC()
	if done != nil {
		b.CompletedAt = done.UTC()
	}
	if expires != nil {
		b.ExpiresAt = expires.UTC()
	}
	return b, nil
}

func (d Deletions) MarkEndUser(ctx context.Context, b *lifecycle.EndUserTombstone) (*lifecycle.EndUserTombstone, error) {
	return scanEndUser(d.Pool.QueryRow(ctx, `INSERT INTO end_user_tombstones
		(business_line, end_user, request_id, requested_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (end_user, business_line) DO UPDATE SET end_user = EXCLUDED.end_user
		RETURNING `+endUserColumns, b.BusinessLine, b.EndUser, b.RequestID, b.RequestedAt))
}

func (d Deletions) GetEndUser(ctx context.Context, businessLine, endUser string) (*lifecycle.EndUserTombstone, error) {
	return scanEndUser(d.Pool.QueryRow(ctx, `SELECT `+endUserColumns+` FROM end_user_tombstones
		WHERE business_line = $1 AND end_user = $2`, businessLine, endUser))
}

func (d Deletions) AddEndUserSessions(ctx context.Context, b *lifecycle.EndUserTombstone, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.Pool.Exec(ctx, `UPDATE end_user_tombstones SET
		session_ids = ARRAY(SELECT DISTINCT unnest(session_ids || $4::text[]) ORDER BY 1),
		revision = revision + cardinality(ARRAY(SELECT DISTINCT unnest(session_ids || $4::text[]))) - cardinality(session_ids),
		completed_at = NULL, expires_at = NULL
		WHERE business_line = $1 AND end_user = $2 AND request_id = $3 AND NOT session_ids @> $4::text[]`,
		b.BusinessLine, b.EndUser, b.RequestID, ids)
	return err
}

func (d Deletions) SealEndUser(ctx context.Context, b *lifecycle.EndUserTombstone) error {
	_, err := d.Pool.Exec(ctx, `UPDATE end_user_tombstones SET sealed = true
		WHERE business_line = $1 AND end_user = $2 AND request_id = $3`, b.BusinessLine, b.EndUser, b.RequestID)
	return err
}

func (d Deletions) PendingEndUsers(ctx context.Context, limit int) ([]*lifecycle.EndUserTombstone, error) {
	rows, err := d.Pool.Query(ctx, `SELECT `+endUserColumns+` FROM end_user_tombstones
		WHERE sealed AND completed_at IS NULL ORDER BY business_line, end_user LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*lifecycle.EndUserTombstone
	for rows.Next() {
		b, err := scanEndUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (d Deletions) CompleteEndUser(ctx context.Context, b *lifecycle.EndUserTombstone, at, expires time.Time) error {
	_, err := d.Pool.Exec(ctx, `UPDATE end_user_tombstones SET completed_at = $5, expires_at = $6
		WHERE business_line = $1 AND end_user = $2 AND request_id = $3 AND revision = $4
		AND sealed AND completed_at IS NULL`, b.BusinessLine, b.EndUser, b.RequestID, b.Revision, at, expires)
	return err
}

func (d Deletions) PruneEndUsers(ctx context.Context, now time.Time, limit int) error {
	// 离线扫描只列键；删除逐键执行，并复查到期时间，避免扫到后又有迟到写入撤回。
	rows, err := d.Pool.Query(ctx, `SELECT business_line, end_user, request_id FROM end_user_tombstones
		WHERE completed_at IS NOT NULL AND expires_at <= $1 ORDER BY expires_at, business_line, end_user LIMIT $2`, now, limit)
	if err != nil {
		return err
	}
	var expired []*lifecycle.EndUserTombstone
	for rows.Next() {
		b := &lifecycle.EndUserTombstone{}
		if err := rows.Scan(&b.BusinessLine, &b.EndUser, &b.RequestID); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range expired {
		if _, err := d.Pool.Exec(ctx, `DELETE FROM end_user_tombstones
			WHERE business_line = $1 AND end_user = $2 AND request_id = $3
			AND completed_at IS NOT NULL AND expires_at <= $4`, b.BusinessLine, b.EndUser, b.RequestID, now); err != nil {
			return err
		}
	}
	return nil
}
