// Package pgsandbox 是沙箱控制器共享状态（去重账本、活跃记录）的 PostgreSQL 实现。
package pgsandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/sandbox"
	"yanshi/sdk/nodesdk"
)

// Ledger 由控制器按 Session 绑定给 SDK；SDK 账本不带 context，这里使用后台 context 并设超时。
type Ledger struct{ Pool *pgxpool.Pool }

var _ sandbox.Ledger = Ledger{}

const ledgerTimeout = 10 * time.Second

func (l Ledger) Get(sessionID, callID string) (*nodesdk.Record, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("sandbox ledger: Session is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	var state int16
	var result []byte
	var at time.Time
	err := l.Pool.QueryRow(ctx, `SELECT state, result, updated_at FROM sandbox_ledger WHERE session_id = $1 AND call_id = $2`, sessionID, callID).Scan(&state, &result, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return &nodesdk.Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	r := &nodesdk.Record{State: nodesdk.State(state), SessionID: sessionID, UpdatedAt: at}
	if result != nil {
		r.Result = &v1.InvokeResult{}
		if err := proto.Unmarshal(result, r.Result); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (l Ledger) Put(sessionID, callID string, r *nodesdk.Record) error {
	if sessionID == "" || r.SessionID != sessionID {
		return fmt.Errorf("sandbox ledger: session mismatch")
	}
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
	at := r.UpdatedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := l.Pool.Exec(ctx, `
		INSERT INTO sandbox_ledger (call_id, state, result, session_id, updated_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (session_id, call_id) DO UPDATE SET state = $2, result = $3, updated_at = $5`,
		callID, int16(r.State), result, sessionID, at)
	return err
}

func (l Ledger) Forget(sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	_, err := l.Pool.Exec(ctx, `DELETE FROM sandbox_ledger WHERE session_id = $1`, sessionID)
	return err
}

func (l Ledger) HasSession(sessionID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	var exists bool
	err := l.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sandbox_ledger WHERE session_id = $1)`, sessionID).Scan(&exists)
	return exists, err
}

func (l Ledger) Prune(before time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout)
	defer cancel()
	return l.prune(ctx, before, false)
}

// PruneContext 让分片维护沿用外层预算；删除成功本身就是下一批的持久续扫位置。
func (l Ledger) PruneContext(ctx context.Context, before time.Time) error {
	return l.prune(ctx, before, true)
}

func (l Ledger) prune(ctx context.Context, before time.Time, bounded bool) error {
	query := `SELECT DISTINCT session_id FROM sandbox_ledger WHERE updated_at < $1 ORDER BY session_id`
	if bounded {
		query += " LIMIT 1000"
	}
	rows, err := l.Pool.Query(ctx, query, before)
	if err != nil {
		return err
	}
	sessions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, sid := range sessions {
		if _, err := l.Pool.Exec(ctx, `DELETE FROM sandbox_ledger WHERE session_id = $1 AND updated_at < $2`, sid, before); err != nil {
			return err
		}
	}
	return nil
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

func (a Activity) Delete(ctx context.Context, id string) error {
	_, err := a.Pool.Exec(ctx, `DELETE FROM sandbox_activity WHERE sandbox_id = $1`, id)
	return err
}

func (a Activity) Contains(ctx context.Context, id string) (bool, error) {
	var has bool
	err := a.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sandbox_activity WHERE sandbox_id = $1)`, id).Scan(&has)
	return has, err
}
