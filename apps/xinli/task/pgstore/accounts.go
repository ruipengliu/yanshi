package pgstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"xinli/account"
	"xinli/pg"
	"xinli/task"
)

func (s *Store) WithOwner(ctx context.Context, owner task.Owner, fn func(context.Context) error) error {
	return pg.WithOwner(ctx, s.pool, owner.BusinessLine, owner.EndUser, fn)
}
func (s *Store) Register(ctx context.Context, identity string, a account.Account) (account.Account, error) {
	tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
	if err != nil {
		return account.Account{}, err
	}
	defer pg.Rollback(tx)
	// 首次注册也按身份串行；不能把同一身份的并发登录分配成两个账号。
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, identity); err != nil {
		return account.Account{}, err
	}
	var payload []byte
	err = tx.QueryRow(ctx, `SELECT a.payload FROM account_identities i JOIN accounts a USING(business_line,end_user) WHERE identity_hash=$1`, identity).Scan(&payload)
	if err == nil {
		var out account.Account
		if err = json.Unmarshal(payload, &out); err != nil {
			return out, err
		}
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return account.Account{}, err
	}
	payload, err = json.Marshal(a)
	if err != nil {
		return account.Account{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO accounts(business_line,end_user,status,payload) VALUES($1,$2,$3,$4)`, a.BusinessLine, a.EndUser, a.Status, payload); err != nil {
		return account.Account{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO account_identities(identity_hash,business_line,end_user) VALUES($1,$2,$3)`, identity, a.BusinessLine, a.EndUser); err != nil {
		return account.Account{}, err
	}
	return a, tx.Commit(ctx)
}

type reader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readAccount(ctx context.Context, db reader, owner task.Owner, lock bool) (account.Record, error) {
	r := account.Record{Devices: map[string]account.Device{}, Sessions: map[string]account.Session{}}
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var payload []byte
	err := db.QueryRow(ctx, `SELECT payload FROM accounts WHERE business_line=$1 AND end_user=$2`+suffix, owner.BusinessLine, owner.EndUser).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, account.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(payload, &r.Account); err != nil {
		return r, err
	}
	for _, table := range []string{"account_devices", "account_sessions"} {
		rows, err := db.Query(ctx, `SELECT payload FROM `+table+` WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser)
		if err != nil {
			return r, err
		}
		for rows.Next() {
			if err = rows.Scan(&payload); err != nil {
				rows.Close()
				return r, err
			}
			if table == "account_devices" {
				var d account.Device
				err = json.Unmarshal(payload, &d)
				r.Devices[d.ID] = d
			} else {
				var ss account.Session
				err = json.Unmarshal(payload, &ss)
				r.Sessions[ss.ID] = ss
			}
			if err != nil {
				rows.Close()
				return r, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return r, err
		}
	}
	err = db.QueryRow(ctx, `SELECT payload FROM account_deletions WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(payload, &r.Deletion)
	return r, err
}
func (s *Store) ReadAccount(ctx context.Context, owner task.Owner) (account.Record, error) {
	var out account.Record
	err := s.WithOwner(ctx, owner, func(ctx context.Context) error {
		var err error
		out, err = readAccount(ctx, pg.Executor(ctx, s.pool), owner, false)
		return err
	})
	return out, err
}
func (s *Store) UpdateAccount(ctx context.Context, owner task.Owner, fn func(*account.Record) error) error {
	return s.WithOwner(ctx, owner, func(ctx context.Context) error {
		tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
		if err != nil {
			return err
		}
		defer pg.Rollback(tx)
		old, err := readAccount(ctx, tx, owner, true)
		if err != nil {
			return err
		}
		next := account.Clone(old)
		if err = fn(&next); err != nil {
			return err
		}
		if !account.ValidChange(old, next) {
			return account.ErrInvalid
		}
		if next.Cleanup {
			if _, err = tx.Exec(ctx, `DELETE FROM outbox_receipts WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE outbox_sessions SET end_user='',gone=true,seen_round=0,
				payload=jsonb_build_object('SessionID',session_id,'Gone',true) WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser); err != nil {
				return err
			}
			for _, table := range []string{"delivery_state", "session_creations", "task_sessions", "orchestration_tasks", "task_sources", "account_identities"} {
				if _, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser); err != nil {
					return err
				}
			}
		}
		payload, err := json.Marshal(next.Account)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE accounts SET status=$3,payload=$4 WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser, next.Account.Status, payload); err != nil {
			return err
		}
		for _, table := range []string{"account_devices", "account_sessions"} {
			if _, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE business_line=$1 AND end_user=$2`, owner.BusinessLine, owner.EndUser); err != nil {
				return err
			}
		}
		for id, d := range next.Devices {
			payload, err = json.Marshal(d)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO account_devices(business_line,end_user,device_id,payload) VALUES($1,$2,$3,$4)`, owner.BusinessLine, owner.EndUser, id, payload); err != nil {
				return err
			}
		}
		for id, ss := range next.Sessions {
			payload, err = json.Marshal(ss)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO account_sessions(business_line,end_user,session_id,access_hash,refresh_hash,payload) VALUES($1,$2,$3,$4,$5,$6)`, owner.BusinessLine, owner.EndUser, id, ss.AccessHash, ss.RefreshHash, payload); err != nil {
				return err
			}
		}
		if next.Deletion.Stage != "" {
			payload, err = json.Marshal(next.Deletion)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO account_deletions(business_line,end_user,payload,receipt_hash) VALUES($1,$2,$3,NULLIF($4,'')) ON CONFLICT(business_line,end_user) DO UPDATE SET payload=EXCLUDED.payload,receipt_hash=EXCLUDED.receipt_hash`, owner.BusinessLine, owner.EndUser, payload, next.Deletion.ReceiptHash); err != nil {
				return err
			}
		}
		for _, t := range next.Enqueue {
			if err = receiveTx(ctx, tx, t.Source, []task.Spec{t.Spec}, t.RetryAt, true); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})
}
func (s *Store) TokenOwner(ctx context.Context, hash string) (task.Owner, error) {
	var out task.Owner
	err := pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT business_line,end_user FROM account_sessions WHERE access_hash=$1 OR refresh_hash=$1`, hash).Scan(&out.BusinessLine, &out.EndUser)
	if errors.Is(err, pgx.ErrNoRows) {
		err = account.ErrNotFound
	}
	return out, err
}

var _ account.Store = (*Store)(nil)

func (s *Store) DeletionOwner(ctx context.Context, hash string) (task.Owner, error) {
	var out task.Owner
	err := pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT business_line,end_user FROM account_deletions WHERE receipt_hash=$1`, hash).Scan(&out.BusinessLine, &out.EndUser)
	if errors.Is(err, pgx.ErrNoRows) {
		err = account.ErrNotFound
	}
	return out, err
}

func (s *Store) AccountOwners(ctx context.Context, line, after string, limit int) ([]task.Owner, error) {
	if line == "" || limit < 1 || limit > 200 {
		return nil, account.ErrInvalid
	}
	rows, err := pg.Executor(ctx, s.pool).Query(ctx, `SELECT end_user FROM accounts
		WHERE business_line=$1 AND end_user COLLATE "C">$2 ORDER BY end_user COLLATE "C" LIMIT $3`, line, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []task.Owner{}
	for rows.Next() {
		owner := task.Owner{BusinessLine: line}
		if err := rows.Scan(&owner.EndUser); err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}

var _ account.Enumerator = (*Store)(nil)
