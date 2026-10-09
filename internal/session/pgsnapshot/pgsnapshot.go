// Package pgsnapshot 是 session.Snapshots 的 PostgreSQL 实现。
package pgsnapshot

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/session"
)

type Store struct{ Pool *pgxpool.Pool }

var _ session.Snapshots = Store{}

func (s Store) Get(ctx context.Context, id string) (*v1.SessionSnapshot, error) {
	var b []byte
	err := s.Pool.QueryRow(ctx, `SELECT data FROM session_snapshots WHERE session_id = $1`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap := &v1.SessionSnapshot{}
	return snap, proto.Unmarshal(b, snap)
}

func (s Store) Put(ctx context.Context, snap *v1.SessionSnapshot) error {
	b, err := proto.Marshal(snap)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO session_snapshots (session_id, seq, data) VALUES ($1, $2, $3)
		ON CONFLICT (session_id) DO UPDATE SET seq = $2, data = $3, updated_at = now()
		WHERE session_snapshots.seq < $2`, snap.GetSessionId(), int64(snap.GetSeq()), b)
	return err
}

func (s Store) Delete(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM session_snapshots WHERE session_id = $1`, id)
	return err
}
