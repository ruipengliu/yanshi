package pgusage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/pg"
	"yanshi/internal/usage"
)

type Tiers struct{ Pool *pgxpool.Pool }

var _ usage.TierStore = Tiers{}

func (s Tiers) Get(ctx context.Context, bl, eu string) (string, error) {
	var tier string
	err := s.Pool.QueryRow(ctx, `SELECT tier FROM end_user_tiers WHERE business_line = $1 AND end_user = $2`, bl, eu).Scan(&tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return tier, err
}

func (s Tiers) Put(ctx context.Context, bl, eu, tier string, at time.Time) error {
	// 同键事务中的写入须沿用调用方连接，否则会逃出回滚，并在单连接池中等待自己。
	_, err := pg.Exec(ctx, s.Pool, `INSERT INTO end_user_tiers (business_line, end_user, tier, updated_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT (business_line, end_user)
		DO UPDATE SET tier = EXCLUDED.tier, updated_at = EXCLUDED.updated_at`, bl, eu, tier, at)
	return err
}

func (s Tiers) Delete(ctx context.Context, bl, eu string) error {
	_, err := pg.Exec(ctx, s.Pool, `DELETE FROM end_user_tiers WHERE business_line = $1 AND end_user = $2`, bl, eu)
	return err
}
