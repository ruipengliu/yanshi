// Package pgusage 是 usage.Store 的 PostgreSQL 实现（迁移 0007_usage.sql）。
package pgusage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/usage"
)

type Store struct{ Pool *pgxpool.Pool }

var _ usage.Store = Store{}

// Record 在一条语句中插入明细，并仅在插入成功（非重复）时累加 EndUser 与业务线合计两行聚合，
// 因此重复记录不会重复计入，聚合与明细也不会出现部分写入。
func (s Store) Record(ctx context.Context, e *usage.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `
		WITH ins AS (
			INSERT INTO usage_entries (id, business_line, end_user, kind, model, input_tokens, output_tokens, sandbox_millis, cost, at, agent)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (id) DO NOTHING
			RETURNING business_line, end_user, date_trunc('hour', at, 'UTC') AS hour, cost, input_tokens, output_tokens, sandbox_millis
		)
		INSERT INTO usage_hourly AS h (business_line, end_user, hour, cost, input_tokens, output_tokens, sandbox_millis)
		SELECT ins.business_line, u.end_user, ins.hour, ins.cost, ins.input_tokens, ins.output_tokens, ins.sandbox_millis
		FROM ins CROSS JOIN LATERAL (VALUES (ins.end_user), ('')) AS u(end_user)
		ON CONFLICT (business_line, end_user, hour) DO UPDATE SET
			cost = h.cost + EXCLUDED.cost, input_tokens = h.input_tokens + EXCLUDED.input_tokens,
			output_tokens = h.output_tokens + EXCLUDED.output_tokens, sandbox_millis = h.sandbox_millis + EXCLUDED.sandbox_millis`,
		e.ID, e.BusinessLine, e.EndUser, string(e.Kind), e.Model, int64(e.InputTokens), int64(e.OutputTokens), int64(e.SandboxMillis), e.Cost, e.At, e.Agent)
	return err
}

func (s Store) Spent(ctx context.Context, businessLine, endUser string, from, to time.Time) (int64, error) {
	var sum int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(sum(cost), 0)::bigint FROM usage_hourly
		WHERE business_line = $1 AND end_user = $2 AND hour >= $3 AND hour < $4`,
		businessLine, endUser, from, to).Scan(&sum)
	return sum, err
}

func (s Store) Report(ctx context.Context, q usage.Query) ([]usage.Row, error) {
	loc := q.Location
	if loc == nil {
		loc = time.UTC
	}
	var key string
	args := []any{q.BusinessLine, q.EndUser, q.From, q.To}
	switch q.GroupBy {
	case usage.ByDay:
		key = `to_char(at AT TIME ZONE $5, 'YYYY-MM-DD')`
		args = append(args, loc.String())
	case usage.ByEndUser:
		key = `end_user`
	case usage.ByModel:
		key = `model`
	case usage.ByAgent:
		key = `agent`
	default:
		return nil, errors.New("usage: unknown group_by")
	}
	// 按字节序排序，与内存实现一致（匿名值 '~' 排在最后）。
	rows, err := s.Pool.Query(ctx, `
		SELECT * FROM (
			SELECT `+key+` AS k, count(*), sum(cost)::bigint, sum(input_tokens)::bigint, sum(output_tokens)::bigint, sum(sandbox_millis)::bigint
			FROM usage_entries
			WHERE business_line = $1 AND ($2 = '' OR end_user = $2) AND at >= $3 AND at < $4
			GROUP BY k
		) t ORDER BY k COLLATE "C"`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (usage.Row, error) {
		var row usage.Row
		var in, out, ms int64
		err := r.Scan(&row.Key, &row.Calls, &row.Cost, &in, &out, &ms)
		row.InputTokens, row.OutputTokens, row.SandboxMillis = uint64(in), uint64(out), uint64(ms)
		return row, err
	})
}

// DeleteEndUser 把明细与聚合中的 EndUser 改为匿名值。聚合以"删除并返回、再合并"的单条语句迁移：
// 并发的 Record 若在其后为该 EndUser 新建了聚合行，对应明细也带着原标识，
// 由 Meter 的先写后查再次匿名化，二者仍然一致。
func (s Store) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	if endUser == "" || endUser == usage.Anonymous {
		return errors.New("usage: invalid end user")
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE usage_entries SET end_user = $3 WHERE business_line = $1 AND end_user = $2`,
			businessLine, endUser, usage.Anonymous); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			WITH moved AS (DELETE FROM usage_hourly WHERE business_line = $1 AND end_user = $2 RETURNING *)
			INSERT INTO usage_hourly AS h (business_line, end_user, hour, cost, input_tokens, output_tokens, sandbox_millis)
			SELECT business_line, $3, hour, cost, input_tokens, output_tokens, sandbox_millis FROM moved
			ON CONFLICT (business_line, end_user, hour) DO UPDATE SET
				cost = h.cost + EXCLUDED.cost, input_tokens = h.input_tokens + EXCLUDED.input_tokens,
				output_tokens = h.output_tokens + EXCLUDED.output_tokens, sandbox_millis = h.sandbox_millis + EXCLUDED.sandbox_millis`,
			businessLine, endUser, usage.Anonymous)
		return err
	})
}

// AnonymizeEntry 在一条语句中改写明细，并把其数量从原 EndUser 的聚合行移到匿名行。
// 并发执行时，后到者在行锁释放后读到已匿名的明细，不再移动。
func (s Store) AnonymizeEntry(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `
		WITH old AS (SELECT id, end_user FROM usage_entries WHERE id = $1 AND end_user <> $2 FOR UPDATE),
		upd AS (
			UPDATE usage_entries e SET end_user = $2 FROM old WHERE e.id = old.id
			RETURNING e.business_line, old.end_user AS prev, date_trunc('hour', e.at, 'UTC') AS hour,
				e.cost, e.input_tokens, e.output_tokens, e.sandbox_millis
		),
		dec AS (
			UPDATE usage_hourly h SET cost = h.cost - upd.cost, input_tokens = h.input_tokens - upd.input_tokens,
				output_tokens = h.output_tokens - upd.output_tokens, sandbox_millis = h.sandbox_millis - upd.sandbox_millis
			FROM upd WHERE h.business_line = upd.business_line AND h.end_user = upd.prev AND h.hour = upd.hour
			RETURNING 1
		)
		INSERT INTO usage_hourly AS h (business_line, end_user, hour, cost, input_tokens, output_tokens, sandbox_millis)
		SELECT business_line, $2, hour, cost, input_tokens, output_tokens, sandbox_millis FROM upd
		ON CONFLICT (business_line, end_user, hour) DO UPDATE SET
			cost = h.cost + EXCLUDED.cost, input_tokens = h.input_tokens + EXCLUDED.input_tokens,
			output_tokens = h.output_tokens + EXCLUDED.output_tokens, sandbox_millis = h.sandbox_millis + EXCLUDED.sandbox_millis`,
		id, usage.Anonymous)
	return err
}

func (s Store) Prune(ctx context.Context, before time.Time) error {
	before = before.Truncate(time.Hour)
	// before 已对齐整点，at < before 与"所在整点早于 before"等价，且能使用索引。
	if _, err := s.Pool.Exec(ctx, `DELETE FROM usage_entries WHERE at < $1`, before); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `DELETE FROM usage_hourly WHERE hour < $1`, before)
	return err
}
