package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"xinli/kernel"
	"xinli/pg"
	"xinli/recovery"
)

func (s *Store) Load(ctx context.Context, line string) (recovery.Progress, error) {
	var payload []byte
	p := recovery.Progress{BusinessLine: line}
	err := pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT payload FROM outbox_recovery WHERE business_line=$1 AND shard=$2`, line, recovery.Shard(ctx)).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(payload, &p)
	return p, err
}

func (s *Store) Session(ctx context.Context, line, id string) (recovery.Checkpoint, error) {
	var payload []byte
	cp := recovery.Checkpoint{SessionID: id}
	err := pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT payload FROM outbox_sessions WHERE business_line=$1 AND session_id=$2`, line, id).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return cp, nil
	}
	if err != nil {
		return cp, err
	}
	err = json.Unmarshal(payload, &cp)
	return cp, err
}

func (s *Store) Known(ctx context.Context, line string, round uint64, after string, limit int) ([]recovery.Checkpoint, error) {
	if limit < 1 {
		return nil, errors.New("recovery: invalid limit")
	}
	rows, err := pg.Executor(ctx, s.pool).Query(ctx, `WITH known AS (
		SELECT session_id,end_user FROM outbox_sessions WHERE business_line=$1
		UNION SELECT session_id COLLATE "C",end_user FROM orchestration_tasks WHERE business_line=$1 AND session_id<>''
		UNION SELECT (payload->>'Session') COLLATE "C",end_user FROM session_creations WHERE business_line=$1 AND COALESCE(payload->>'Session','')<>''
	) SELECT k.session_id,k.end_user,c.payload FROM known k
		LEFT JOIN outbox_sessions c ON c.business_line=$1 AND c.session_id=k.session_id
		LEFT JOIN accounts a ON a.business_line=$1 AND a.end_user=k.end_user
		WHERE k.session_id>$2 AND NOT COALESCE(c.gone,false) AND ($5 OR c.session_id IS NULL OR c.seen_round<>$3)
		AND (a.status IS NULL OR a.status<>'deleted') ORDER BY k.session_id LIMIT $4`, line, after, int64(round), limit, recovery.Shard(ctx) >= 0)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []recovery.Checkpoint{}
	for rows.Next() {
		var payload []byte
		var cp recovery.Checkpoint
		if err := rows.Scan(&cp.SessionID, &cp.EndUser, &payload); err != nil {
			return nil, err
		}
		if payload != nil {
			if err := json.Unmarshal(payload, &cp); err != nil {
				return nil, err
			}
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

func (s *Store) Commit(ctx context.Context, expected uint64, p recovery.Progress, batch recovery.Batch) error {
	if p.Version != expected+1 {
		return recovery.ErrConflict
	}
	if err := recovery.ValidateBatch(p.BusinessLine, batch); err != nil {
		return err
	}
	tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
	if err != nil {
		return err
	}
	defer pg.Rollback(tx)
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_recovery(business_line,version,payload) VALUES($1,0,'{}') ON CONFLICT DO NOTHING`, p.BusinessLine); err != nil {
		return err
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT version FROM outbox_recovery WHERE business_line=$1 AND shard=-1 FOR UPDATE`, p.BusinessLine).Scan(&version); err != nil {
		return err
	}
	if id := recovery.Shard(ctx); id >= 0 {
		if _, err = tx.Exec(ctx, `INSERT INTO outbox_recovery(business_line,shard,version,payload) VALUES($1,$2,0,'{}') ON CONFLICT DO NOTHING`, p.BusinessLine, id); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT version FROM outbox_recovery WHERE business_line=$1 AND shard=$2 FOR UPDATE`, p.BusinessLine, id).Scan(&version); err != nil {
			return err
		}
	}
	if uint64(version) != expected {
		return recovery.ErrConflict
	}
	// 账号终态检查与写入同事务，并与注销 Cleanup 的账号行锁互斥。
	deleted := func(user string) (bool, error) {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM accounts WHERE business_line=$1 AND end_user=$2 FOR UPDATE`, p.BusinessLine, user).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return status == "deleted", err
	}
	if cp := batch.Checkpoint; cp != nil {
		gone, err := deleted(cp.EndUser)
		if err != nil {
			return err
		}
		if !gone {
			payload, err := json.Marshal(cp)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO outbox_sessions(business_line,session_id,end_user,seen_round,payload)
				VALUES($1,$2,$3,$4,$5) ON CONFLICT(business_line,session_id) DO UPDATE
				SET end_user=EXCLUDED.end_user,seen_round=EXCLUDED.seen_round,payload=EXCLUDED.payload WHERE NOT outbox_sessions.gone`,
				p.BusinessLine, cp.SessionID, cp.EndUser, int64(cp.SeenRound), payload)
			if err != nil {
				return err
			}
		}
	}
	for _, e := range batch.Envelopes {
		if e.Kind == "session_deleted" {
			if err := forgetRecovery(ctx, tx, p.BusinessLine, e.SessionID); err != nil {
				return err
			}
		} else {
			gone, err := deleted(e.EndUser)
			if err != nil {
				return err
			}
			if gone {
				continue
			}
			cp := recovery.Checkpoint{SessionID: e.SessionID, EndUser: e.EndUser}
			payload, err := json.Marshal(cp)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO outbox_sessions(business_line,session_id,end_user,payload)
				VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, p.BusinessLine, e.SessionID, e.EndUser, payload); err != nil {
				return err
			}
			if err = tx.QueryRow(ctx, `SELECT gone FROM outbox_sessions WHERE business_line=$1 AND session_id=$2`, p.BusinessLine, e.SessionID).Scan(&gone); err != nil {
				return err
			}
			if gone {
				continue
			}
		}
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		// 稀疏信封只登记已知 Session，不推进完整日志的 seq 检查点。
		_, err = tx.Exec(ctx, `INSERT INTO outbox_receipts(business_line,session_id,seq,end_user,envelope_id,payload)
			VALUES($1,$2,$3::text::numeric,$4,NULLIF($5,''),$6) ON CONFLICT(business_line,session_id,seq) DO UPDATE
			SET envelope_id=EXCLUDED.envelope_id,payload=EXCLUDED.payload WHERE outbox_receipts.envelope_id IS NULL`,
			p.BusinessLine, e.SessionID, strconv.FormatUint(e.Seq, 10), e.EndUser, e.EnvelopeID, payload)
		if err != nil {
			return err
		}
	}
	if batch.Inaccessible != "" {
		if err := forgetRecovery(ctx, tx, p.BusinessLine, batch.Inaccessible); err != nil {
			return err
		}
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE outbox_recovery SET version=$2,payload=$3 WHERE business_line=$1 AND shard=$4`, p.BusinessLine, int64(p.Version), payload, recovery.Shard(ctx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func forgetRecovery(ctx context.Context, tx pgx.Tx, line, id string) error {
	if err := forgetDelivery(ctx, tx, line, id); err != nil {
		return err
	}
	payload, err := json.Marshal(recovery.Checkpoint{SessionID: id, Gone: true})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_sessions(business_line,session_id,end_user,gone,payload) VALUES($1,$2,'',true,$3)
		ON CONFLICT(business_line,session_id) DO UPDATE SET end_user='',gone=true,seen_round=0,payload=EXCLUDED.payload`, line, id, payload); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM outbox_receipts WHERE business_line=$1 AND session_id=$2 AND seq<>0`, line, id)
	return err
}

func (s *Store) Recorded(ctx context.Context, line string) ([]kernel.Envelope, error) {
	rows, err := pg.Executor(ctx, s.pool).Query(ctx, `SELECT payload FROM outbox_receipts WHERE business_line=$1 ORDER BY session_id,seq`, line)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []kernel.Envelope{}
	for rows.Next() {
		var payload []byte
		var e kernel.Envelope
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

var _ recovery.Consumer = (*Store)(nil)
