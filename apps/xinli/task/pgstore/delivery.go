package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"xinli/account"
	"xinli/delivery"
	"xinli/kernel"
	"xinli/pg"
	"xinli/recovery"
	"xinli/task"
)

func (s *Store) Pending(ctx context.Context, line string, limit int) ([]kernel.Envelope, error) {
	if limit < 1 {
		return nil, task.ErrInvalid
	}
	rows, err := pg.Executor(ctx, s.pool).Query(ctx, `SELECT payload FROM outbox_receipts WHERE business_line=$1 AND NOT processed ORDER BY session_id,seq LIMIT $2`, line, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []kernel.Envelope{}
	for rows.Next() {
		var data []byte
		var e kernel.Envelope
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func readDelivery(ctx context.Context, db pg.DB, owner task.Owner) (delivery.State, error) {
	var data []byte
	var state delivery.State
	err := db.QueryRow(ctx, `SELECT payload FROM delivery_state WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	} else if err == nil {
		err = json.Unmarshal(data, &state)
	}
	state.Init()
	return state, err
}

func saveDelivery(ctx context.Context, tx pgx.Tx, owner task.Owner, state delivery.State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_state(business_line,end_user,payload) VALUES($1,$2,$3)
		ON CONFLICT(business_line,end_user) DO UPDATE SET payload=EXCLUDED.payload`, owner.BusinessLine, owner.EndUser, data)
	return err
}

func activeDelivery(ctx context.Context, db pg.DB, owner task.Owner) error {
	var status string
	err := db.QueryRow(ctx, `SELECT status FROM accounts WHERE business_line=$1 AND end_user=$2 FOR UPDATE`, owner.BusinessLine, owner.EndUser).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && status != string(account.Active) {
		return account.ErrInactive
	}
	return err
}

func (s *Store) Process(ctx context.Context, e kernel.Envelope, now time.Time) error {
	tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
	if err != nil {
		return err
	}
	defer pg.Rollback(tx)
	// 与接收及删除使用同一业务线锁序，防止读取旧接收行后复活已删 Session。
	var version int64
	if err = tx.QueryRow(ctx, `SELECT version FROM outbox_recovery WHERE business_line=$1 FOR UPDATE`, e.BusinessLine).Scan(&version); err != nil {
		return err
	}
	var data []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM outbox_receipts WHERE business_line=$1 AND session_id=$2 AND seq=$3::text::numeric AND NOT processed`, e.BusinessLine, e.SessionID, strconv.FormatUint(e.Seq, 10)).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &e); err != nil {
		return err
	}
	owner := task.Owner{BusinessLine: e.BusinessLine, EndUser: e.EndUser}
	if e.EndUser != "" {
		err = activeDelivery(ctx, tx, owner)
		if err != nil && !errors.Is(err, account.ErrInactive) {
			return err
		}
		if err == nil {
			state, err := readDelivery(ctx, tx, owner)
			if err != nil {
				return err
			}
			state.Receive(e, now)
			if err = saveDelivery(ctx, tx, owner, state); err != nil {
				return err
			}
		}
	}
	if e.Kind == "run_completed" || e.Kind == "run_failed" {
		_, err = tx.Exec(ctx, `UPDATE orchestration_tasks SET retry_at=$5 WHERE business_line=$1 AND end_user=$2 AND session_id=$3
			AND state IN ('accepted','unknown') AND (COALESCE(receipt->>'run_id','')='' OR receipt->>'run_id'=$4)`, e.BusinessLine, e.EndUser, e.SessionID, e.RunID, now)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE outbox_receipts SET processed=true WHERE business_line=$1 AND session_id=$2 AND seq=$3::text::numeric`, e.BusinessLine, e.SessionID, strconv.FormatUint(e.Seq, 10)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReadDelivery(ctx context.Context, owner task.Owner) (delivery.State, error) {
	var out delivery.State
	err := s.UpdateDelivery(ctx, owner, func(state *delivery.State) error { out = delivery.Clone(*state); return nil })
	return out, err
}

func (s *Store) UpdateDelivery(ctx context.Context, owner task.Owner, fn func(*delivery.State) error) error {
	tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
	if err != nil {
		return err
	}
	defer pg.Rollback(tx)
	// 与删除先确定 Session 归属、后清通知的事务串行，堵住首次渠道登记与删除交错。
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_recovery(business_line,version,payload) VALUES($1,0,'{}') ON CONFLICT DO NOTHING`, owner.BusinessLine); err != nil {
		return err
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT version FROM outbox_recovery WHERE business_line=$1 FOR UPDATE`, owner.BusinessLine).Scan(&version); err != nil {
		return err
	}
	if err = activeDelivery(ctx, tx, owner); err != nil {
		return err
	}
	state, err := readDelivery(ctx, tx, owner)
	if err != nil {
		return err
	}
	if err = fn(&state); err != nil {
		return err
	}
	// 按本次状态里的 Session 逐 ID 复查 Gone，不扫描其他账号的数据。
	ids := map[string]bool{}
	for _, item := range state.Items {
		ids[item.SessionID] = true
	}
	for _, job := range state.Jobs {
		if job.Item.SessionID != "" {
			ids[job.Item.SessionID] = true
		}
	}
	for _, id := range state.Inbound {
		ids[id] = true
	}
	for id := range ids {
		payload, err := json.Marshal(recovery.Checkpoint{SessionID: id, EndUser: owner.EndUser})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO outbox_sessions(business_line,session_id,end_user,payload) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, owner.BusinessLine, id, owner.EndUser, payload); err != nil {
			return err
		}
		var gone bool
		err = tx.QueryRow(ctx, `SELECT gone FROM outbox_sessions WHERE business_line=$1 AND session_id=$2`, owner.BusinessLine, id).Scan(&gone)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if gone {
			state.Forget(id)
		}
	}
	if err = saveDelivery(ctx, tx, owner, state); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// 删除与普通接收同事务；owner 行锁也串行化通知状态回写。
func forgetDelivery(ctx context.Context, tx pgx.Tx, line, session string) error {
	var user string
	err := tx.QueryRow(ctx, `SELECT end_user FROM outbox_sessions WHERE business_line=$1 AND session_id=$2`, line, session).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && user == "" {
		return nil
	}
	if err != nil {
		return err
	}
	owner := task.Owner{BusinessLine: line, EndUser: user}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM accounts WHERE business_line=$1 AND end_user=$2 FOR UPDATE`, line, user).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	state, err := readDelivery(ctx, tx, owner)
	if err != nil {
		return err
	}
	state.Forget(session)
	return saveDelivery(ctx, tx, owner, state)
}

var _ delivery.Store = (*Store)(nil)
