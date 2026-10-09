// Package pglifecycle 是 lifecycle.Index 与 lifecycle.Deletions 的 PostgreSQL 实现。
package pglifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/lifecycle"
)

type Index struct{ Pool *pgxpool.Pool }

var _ lifecycle.Index = Index{}

func (x Index) Put(ctx context.Context, s *lifecycle.Session) error {
	_, err := x.Pool.Exec(ctx, `
		INSERT INTO sessions (session_id, business_line, end_user, agent, created_at, last_input_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (session_id) DO NOTHING`,
		s.ID, s.BusinessLine, s.EndUser, s.Agent, s.CreatedAt, s.LastInputAt)
	return err
}

func (x Index) Touch(ctx context.Context, id string, at time.Time) error {
	_, err := x.Pool.Exec(ctx, `UPDATE sessions SET last_input_at = $2 WHERE session_id = $1 AND last_input_at < $2`, id, at)
	return err
}

func (x Index) MarkClosed(ctx context.Context, id string, at time.Time) error {
	_, err := x.Pool.Exec(ctx, `UPDATE sessions SET closed_at = $2 WHERE session_id = $1 AND closed_at IS NULL`, id, at)
	return err
}

const columns = `session_id, business_line, end_user, agent, created_at, last_input_at, closed_at`

func scan(row pgx.CollectableRow) (*lifecycle.Session, error) {
	s := &lifecycle.Session{}
	var closed *time.Time
	if err := row.Scan(&s.ID, &s.BusinessLine, &s.EndUser, &s.Agent, &s.CreatedAt, &s.LastInputAt, &closed); err != nil {
		return nil, err
	}
	s.CreatedAt, s.LastInputAt = s.CreatedAt.UTC(), s.LastInputAt.UTC()
	if closed != nil {
		s.ClosedAt = closed.UTC()
	}
	return s, nil
}

func (x Index) Get(ctx context.Context, id string) (*lifecycle.Session, error) {
	rows, err := x.Pool.Query(ctx, `SELECT `+columns+` FROM sessions WHERE session_id = $1`, id)
	if err != nil {
		return nil, err
	}
	s, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, lifecycle.ErrNotFound
	}
	return s, err
}

func (x Index) List(ctx context.Context, businessLine, endUser, cursor string, limit int) ([]*lifecycle.Session, string, error) {
	q := `SELECT ` + columns + ` FROM sessions WHERE business_line = $1 AND ($2 = '' OR end_user = $2)`
	args := []any{businessLine, endUser, limit + 1}
	if cursor != "" {
		t, id, err := lifecycle.DecodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		q += ` AND (last_input_at, session_id) < ($4, $5)`
		args = append(args, t, id)
	}
	rows, err := x.Pool.Query(ctx, q+` ORDER BY last_input_at DESC, session_id DESC LIMIT $3`, args...)
	if err != nil {
		return nil, "", err
	}
	page, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, "", err
	}
	if len(page) <= limit {
		return page, "", nil
	}
	return page[:limit], lifecycle.EncodeCursor(page[limit-1]), nil
}

func (x Index) ids(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := x.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (x Index) IDsOf(ctx context.Context, businessLine, endUser string) ([]string, error) {
	return x.ids(ctx, `SELECT session_id FROM sessions WHERE business_line = $1 AND end_user = $2`, businessLine, endUser)
}

func (x Index) IdleBefore(ctx context.Context, businessLine string, t time.Time, limit int) ([]string, error) {
	return x.ids(ctx, `SELECT session_id FROM sessions WHERE business_line = $1 AND closed_at IS NULL AND last_input_at < $2
		ORDER BY session_id LIMIT $3`, businessLine, t, limit)
}

func (x Index) ClosedBefore(ctx context.Context, businessLine string, t time.Time, limit int) ([]string, error) {
	return x.ids(ctx, `SELECT session_id FROM sessions WHERE business_line = $1 AND closed_at < $2
		ORDER BY session_id LIMIT $3`, businessLine, t, limit)
}

func (x Index) Delete(ctx context.Context, id string) error {
	_, err := x.Pool.Exec(ctx, `DELETE FROM sessions WHERE session_id = $1`, id)
	return err
}

type Deletions struct{ Pool *pgxpool.Pool }

var _ lifecycle.Deletions = Deletions{}

func (d Deletions) Mark(ctx context.Context, t *lifecycle.Tombstone) (bool, error) {
	var req *string
	if t.RequestID != "" {
		req = &t.RequestID
	}
	tag, err := d.Pool.Exec(ctx, `
		INSERT INTO session_tombstones (session_id, business_line, reason, request_id, requested_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (session_id) DO NOTHING`,
		t.SessionID, t.BusinessLine, t.Reason, req, t.RequestedAt)
	return tag.RowsAffected() == 1, err
}

func (d Deletions) Get(ctx context.Context, id string) (*lifecycle.Tombstone, error) {
	t := &lifecycle.Tombstone{SessionID: id}
	var req *string
	var done *time.Time
	err := d.Pool.QueryRow(ctx, `SELECT business_line, reason, request_id, requested_at, completed_at
		FROM session_tombstones WHERE session_id = $1`, id).Scan(&t.BusinessLine, &t.Reason, &req, &t.RequestedAt, &done)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, lifecycle.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.RequestedAt = t.RequestedAt.UTC()
	if req != nil {
		t.RequestID = *req
	}
	if done != nil {
		t.CompletedAt = done.UTC()
	}
	return t, nil
}

func (d Deletions) Complete(ctx context.Context, id string, at time.Time) error {
	_, err := d.Pool.Exec(ctx, `UPDATE session_tombstones SET completed_at = $2 WHERE session_id = $1 AND completed_at IS NULL`, id, at)
	return err
}

func (d Deletions) Pending(ctx context.Context, limit int) ([]string, error) {
	rows, err := d.Pool.Query(ctx, `SELECT session_id FROM session_tombstones WHERE completed_at IS NULL ORDER BY session_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (d Deletions) CreateRequest(ctx context.Context, r *lifecycle.Request) error {
	_, err := d.Pool.Exec(ctx, `INSERT INTO deletion_requests (request_id, business_line, created_at) VALUES ($1, $2, $3)
		ON CONFLICT (request_id) DO NOTHING`, r.ID, r.BusinessLine, r.CreatedAt)
	return err
}

func (d Deletions) Request(ctx context.Context, id string) (*lifecycle.Request, error) {
	r := &lifecycle.Request{ID: id}
	err := d.Pool.QueryRow(ctx, `
		SELECT r.business_line, r.created_at, count(t.session_id), count(t.completed_at)
		FROM deletion_requests r LEFT JOIN session_tombstones t ON t.request_id = r.request_id
		WHERE r.request_id = $1 GROUP BY r.business_line, r.created_at`, id).Scan(&r.BusinessLine, &r.CreatedAt, &r.Sessions, &r.Completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, lifecycle.ErrNotFound
	}
	r.CreatedAt = r.CreatedAt.UTC()
	return r, err
}
