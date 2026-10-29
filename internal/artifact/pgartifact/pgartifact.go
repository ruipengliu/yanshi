// Package pgartifact 是 artifact.MetaStore 的 PostgreSQL 实现。
package pgartifact

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/artifact"
	"yanshi/internal/pg"
	"yanshi/internal/shard"
)

type Meta struct {
	Pool   *pgxpool.Pool
	Select pg.PoolSelector
}

var _ artifact.MetaStore = Meta{}

func (s Meta) Create(ctx context.Context, m *artifact.Meta) error {
	pool, err := pg.SelectPool(s.Select, s.Pool, shard.Target{Kind: shard.SessionData, Key: m.SessionID})
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `INSERT INTO artifacts (id, session_id, name, mime_type, size, sha256, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, m.ID, m.SessionID, m.Name, m.MimeType, m.Size, m.SHA256, m.CreatedAt)
	return err
}

const cols = `id, session_id, name, mime_type, size, sha256, created_at`

func scan(r pgx.CollectableRow) (*artifact.Meta, error) {
	m := &artifact.Meta{}
	return m, r.Scan(&m.ID, &m.SessionID, &m.Name, &m.MimeType, &m.Size, &m.SHA256, &m.CreatedAt)
}

func (s Meta) Lookup(ctx context.Context, id string) (*artifact.Meta, error) {
	pool, err := pg.SelectPool(s.Select, s.Pool, shard.Target{Kind: shard.Object, Key: id})
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT `+cols+` FROM artifacts WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, artifact.ErrNotFound
	}
	return m, err
}

func (s Meta) List(ctx context.Context, sessionID string) ([]*artifact.Meta, error) {
	pool, err := pg.SelectPool(s.Select, s.Pool, shard.Target{Kind: shard.SessionData, Key: sessionID})
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT `+cols+` FROM artifacts WHERE session_id = $1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

func (s Meta) Delete(ctx context.Context, sessionID, id string) error {
	pool, err := pg.SelectPool(s.Select, s.Pool, shard.Target{Kind: shard.SessionData, Key: sessionID})
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `DELETE FROM artifacts WHERE id = $1 AND session_id = $2`, id, sessionID)
	return err
}

func (s Meta) Get(ctx context.Context, sessionID, id string) (*artifact.Meta, error) {
	pool, err := pg.SelectPool(s.Select, s.Pool, shard.Target{Kind: shard.SessionData, Key: sessionID})
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT `+cols+` FROM artifacts WHERE id = $1 AND session_id = $2`, id, sessionID)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, artifact.ErrNotFound
	}
	return m, err
}
