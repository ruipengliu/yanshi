// Package pgsandbox 是沙箱控制器共享状态（去重账本、活跃记录）的 PostgreSQL 实现。
package pgsandbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/sandbox"
	"yanshi/sdk/nodesdk"
)

// Ledger 实现 nodesdk.Ledger。nodesdk.Ledger 不带 context，这里使用后台 context 并设超时。
type Ledger struct{ Pool *pgxpool.Pool }

var _ nodesdk.Ledger = Ledger{}

const ledgerTimeout = 10 * time.Second

func (l Ledger) Get(callID string) (*nodesdk.Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	var state int16
	var result []byte
	err := l.Pool.QueryRow(ctx, `SELECT state, result FROM sandbox_ledger WHERE call_id = $1`, callID).Scan(&state, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return &nodesdk.Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	r := &nodesdk.Record{State: nodesdk.State(state)}
	if result != nil {
		r.Result = &v1.InvokeResult{}
		if err := proto.Unmarshal(result, r.Result); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (l Ledger) Put(callID string, r *nodesdk.Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	var result []byte
	if r.Result != nil {
		b, err := proto.Marshal(r.Result)
		if err != nil {
			return err
		}
		result = b
	}
	_, err := l.Pool.Exec(ctx, `
		INSERT INTO sandbox_ledger (call_id, state, result) VALUES ($1, $2, $3)
		ON CONFLICT (call_id) DO UPDATE SET state = $2, result = $3`, callID, int16(r.State), result)
	return err
}

type Activity struct{ Pool *pgxpool.Pool }

var _ sandbox.Activity = Activity{}

func (a Activity) Touch(ctx context.Context, id string, at time.Time) error {
	_, err := a.Pool.Exec(ctx, `
		INSERT INTO sandbox_activity (sandbox_id, last_active) VALUES ($1, $2)
		ON CONFLICT (sandbox_id) DO UPDATE SET last_active = greatest(sandbox_activity.last_active, $2)`, id, at)
	return err
}

func (a Activity) ClaimIdle(ctx context.Context, before time.Time, limit int) ([]string, error) {
	rows, err := a.Pool.Query(ctx, `
		DELETE FROM sandbox_activity WHERE sandbox_id IN (
			SELECT sandbox_id FROM sandbox_activity WHERE last_active < $1
			ORDER BY sandbox_id LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING sandbox_id`, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
