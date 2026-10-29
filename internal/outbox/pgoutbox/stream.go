package pgoutbox

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/outbox"
	"yanshi/internal/pg"
)

// snapshot 的元数据与行查询共用 REPEATABLE READ 快照；清理推进地板时不会读到新旧混合状态。
func snapshot(ctx context.Context, tx pgx.Tx) (outbox.StreamState, uint64, error) {
	var state outbox.StreamState
	var key, floor, target, xmin string
	err := tx.QueryRow(ctx, `SELECT stream_epoch, cursor_key, floor::text, prune_target::text,
		pg_snapshot_xmin(pg_current_snapshot())::text FROM outbox_stream WHERE singleton`).Scan(
		&state.Epoch, &key, &floor, &target, &xmin)
	if err != nil {
		return state, 0, err
	}
	state.Key = []byte(key)
	state.Floor.XID, err = strconv.ParseUint(floor, 10, 64)
	if err != nil {
		return state, 0, err
	}
	state.Target.XID, err = strconv.ParseUint(target, 10, 64)
	if err != nil {
		return state, 0, err
	}
	watermark, err := strconv.ParseUint(xmin, 10, 64)
	return state, watermark, err
}

func xid(n uint64) string { return strconv.FormatUint(n, 10) }

func (s Store) Read(ctx context.Context, req outbox.Request) (outbox.Page, error) {
	r, err := req.Normalize()
	if req.Shard != s.Shard {
		err = outbox.ErrShard
	}
	if err != nil {
		return outbox.Page{}, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return outbox.Page{}, err
	}
	defer tx.Rollback(ctx)
	state, xmin, err := snapshot(ctx, tx)
	if err != nil {
		return outbox.Page{}, err
	}
	start, err := state.Start(r)
	if err != nil {
		return outbox.Page{}, err
	}
	watermark := outbox.Position{XID: xmin}
	if watermark.Before(start) {
		return outbox.Page{}, outbox.ErrCursor
	}
	kinds := make([]string, len(r.Kinds))
	for i, k := range r.Kinds {
		kinds[i] = string(k)
	}
	rows, err := tx.Query(ctx, `SELECT xid::text, id, envelope_id, business_line, session_id, coalesce(seq,0), kind,
		coalesce(end_user,''), coalesce(run_id,''), coalesce(call_id,''), coalesce(agent,''), coalesce(agent_version,''),
		coalesce(reason,''), coalesce(deletion_request_id,'') FROM outbox
		WHERE business_line=$1 AND xid < $2::xid8 AND (xid,id) > ($3::xid8,$4::bigint)
		AND kind = ANY($5::text[]) ORDER BY outbox.xid,outbox.id LIMIT $6`,
		r.BusinessLine, xid(xmin), xid(start.XID), int64(start.ID), kinds, r.Limit)
	if err != nil {
		return outbox.Page{}, err
	}
	defer rows.Close()
	page := outbox.Page{Events: []outbox.Envelope{}}
	next := watermark
	for rows.Next() {
		var e outbox.Envelope
		var x string
		var id uint64
		if err := rows.Scan(&x, &id, &e.EnvelopeID, &e.BusinessLine, &e.SessionID, &e.Seq, &e.Kind,
			&e.EndUser, &e.RunID, &e.CallID, &e.Agent, &e.AgentVersion, &e.Reason, &e.DeletionRequestID); err != nil {
			return outbox.Page{}, err
		}
		page.Events = append(page.Events, e)
		if len(page.Events) == r.Limit {
			xnum, err := strconv.ParseUint(x, 10, 64)
			if err != nil {
				return outbox.Page{}, err
			}
			next = outbox.Position{XID: xnum, ID: id}
		}
	}
	if err := rows.Err(); err != nil {
		return outbox.Page{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return outbox.Page{}, err
	}
	page.Next = state.Encode(r, next)
	metrics.OutboxConsumerLag.WithLabelValues(strconv.Itoa(s.Shard), r.BusinessLine).Set(float64(xmin - next.XID))
	return page, nil
}

func (s Store) Head(ctx context.Context, req outbox.Request) (string, error) {
	r, err := req.Normalize()
	if req.Shard != s.Shard {
		err = outbox.ErrShard
	}
	if err != nil {
		return "", err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	state, xmin, err := snapshot(ctx, tx)
	if err != nil {
		return "", err
	}
	if _, err := state.Start(r); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return state.Encode(r, outbox.Position{XID: xmin}), nil
}

func (s Store) Subscribe(bl string) (<-chan struct{}, func()) {
	if s.Notifier == nil {
		return nil, func() {}
	}
	return s.Notifier.Subscribe(pg.TopicOutbox, bl)
}

// Maintain 先持久化清理目标，再按 Session 删除有限行，最后在无剩余行时推进地板。
// 目标以下的事务早已结束，不会有迟到插入；中断可重做，多 Janitor 可并发执行。
func (s Store) Maintain(ctx context.Context, clk clock.Clock, batch int) error {
	if batch <= 0 {
		return outbox.ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var epoch, floor, target string
	if err := tx.QueryRow(ctx, `SELECT stream_epoch, floor::text, prune_target::text FROM outbox_stream WHERE singleton FOR UPDATE`).Scan(&epoch, &floor, &target); err != nil {
		return err
	}
	var observed string
	if err := tx.QueryRow(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text`).Scan(&observed); err != nil {
		return err
	}
	// 在取得水位后取时钟；等待连接或元数据锁的时间不能算进可发布保留期。
	now := clk.Now()
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_checkpoints (safe_xmin,observed_at)
		VALUES ($1::xid8,$2) ON CONFLICT (safe_xmin) DO NOTHING`, observed, now); err != nil {
		return err
	}
	if target == "0" {
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT safe_xmin FROM outbox_checkpoints
			WHERE observed_at <= $1 AND safe_xmin > $2::xid8 ORDER BY safe_xmin DESC LIMIT 1),'0'::xid8)::text`, now.Add(-outbox.Retention), floor).Scan(&target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox_stream SET prune_target=$1::xid8 WHERE singleton`, target); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if target == "0" {
		return nil
	}

	// 候选扫描是分片级离线路径，实际删除语句只作用于一个 Session。
	rows, err := s.Pool.Query(ctx, `SELECT session_id,id FROM outbox WHERE xid < $1::xid8 ORDER BY xid,id LIMIT $2`, target, batch)
	if err != nil {
		return err
	}
	type candidate struct {
		sid string
		id  int64
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.sid, &c.id)
		return c, err
	})
	if err != nil {
		return err
	}
	bySession := map[string][]int64{}
	var sessions []string
	for _, c := range candidates {
		if _, ok := bySession[c.sid]; !ok {
			sessions = append(sessions, c.sid)
		}
		bySession[c.sid] = append(bySession[c.sid], c.id)
	}
	for _, sid := range sessions {
		// 世代检查与元数据锁防止重建后的旧清理者继续删除新流。
		del, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		var current string
		err = del.QueryRow(ctx, `SELECT stream_epoch FROM outbox_stream WHERE singleton FOR SHARE`).Scan(&current)
		if err == nil && current == epoch {
			_, err = del.Exec(ctx, `DELETE FROM outbox WHERE session_id=$1 AND id=ANY($2::bigint[]) AND xid < $3::xid8`, sid, bySession[sid], target)
		}
		if err != nil || current != epoch {
			del.Rollback(ctx)
			if err != nil {
				return err
			}
			return outbox.ErrEpoch
		}
		if err := del.Commit(ctx); err != nil {
			return err
		}
	}
	// 删除与最终地板不是一个大事务；Target 保证中途读取明确过期，而不是静默漏信封。
	finish, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer finish.Rollback(ctx)
	var current, pending string
	if err := finish.QueryRow(ctx, `SELECT stream_epoch,prune_target::text FROM outbox_stream WHERE singleton FOR UPDATE`).Scan(&current, &pending); err != nil {
		return err
	}
	if current != epoch {
		return outbox.ErrEpoch
	}
	if pending != target {
		return nil
	}
	var remains bool
	if err := finish.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbox WHERE xid < $1::xid8)`, target).Scan(&remains); err != nil {
		return err
	}
	if !remains {
		if _, err := finish.Exec(ctx, `UPDATE outbox_stream SET floor=$1::xid8,prune_target='0' WHERE singleton`, target); err != nil {
			return err
		}
		if _, err := finish.Exec(ctx, `DELETE FROM outbox_checkpoints WHERE safe_xmin <= $1::xid8`, target); err != nil {
			return err
		}
	}
	return finish.Commit(ctx)
}

// RotateEpoch 必须在恢复数据库、重新开放服务之前执行；重启本身不调用它。
func (s Store) RotateEpoch(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE outbox_stream SET stream_epoch=gen_random_uuid()::text,
		cursor_key=gen_random_uuid()::text, floor='0', prune_target='0' WHERE singleton`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM outbox_checkpoints`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s Store) Stats(ctx context.Context) (outbox.Stats, error) {
	var st outbox.Stats
	var age float64
	err := s.Pool.QueryRow(ctx, `SELECT
		(pg_snapshot_xmax(pg_current_snapshot())::text::numeric - pg_snapshot_xmin(pg_current_snapshot())::text::numeric)::bigint,
		coalesce((SELECT greatest(0,extract(epoch FROM clock_timestamp()-min(xact_start))) FROM pg_stat_activity
			WHERE datname=current_database() AND xact_start IS NOT NULL AND pid<>pg_backend_pid()),0)::float8,
		(SELECT count(*) FROM outbox), pg_total_relation_size('outbox')`).Scan(&st.WatermarkLag, &age, &st.Rows, &st.Bytes)
	st.OldestTransaction = time.Duration(age * float64(time.Second))
	return st, err
}
