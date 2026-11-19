// Package pgconsent 是 consent.Store 的 PostgreSQL 实现（表 end_user_consents，分片键为 EndUser）。
package pgconsent

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/consent"
	"yanshi/internal/pg"
)

type Store struct{ Pool *pgxpool.Pool }

var _ consent.Store = Store{}

func (s Store) Get(ctx context.Context, bl, eu string, kind consent.Kind) (time.Time, error) {
	var at time.Time
	err := s.Pool.QueryRow(ctx, `SELECT granted_at FROM end_user_consents WHERE business_line = $1 AND end_user = $2 AND kind = $3`,
		bl, eu, string(kind)).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return at, err
}

func (s Store) Put(ctx context.Context, bl, eu string, kind consent.Kind, at time.Time) error {
	// 同键事务中的写入须沿用调用方连接（pg.Exec）。已有记录时保留原同意时间。
	_, err := pg.Exec(ctx, s.Pool, `INSERT INTO end_user_consents (business_line, end_user, kind, granted_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT (business_line, end_user, kind) DO NOTHING`, bl, eu, string(kind), at)
	return err
}

func (s Store) Delete(ctx context.Context, bl, eu string, kind consent.Kind) error {
	_, err := pg.Exec(ctx, s.Pool, `DELETE FROM end_user_consents WHERE business_line = $1 AND end_user = $2 AND kind = $3`, bl, eu, string(kind))
	return err
}

func (s Store) DeleteEndUser(ctx context.Context, bl, eu string) error {
	_, err := pg.Exec(ctx, s.Pool, `DELETE FROM end_user_consents WHERE business_line = $1 AND end_user = $2`, bl, eu)
	return err
}
