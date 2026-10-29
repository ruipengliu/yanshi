// Package pgstore 只使用 xinli 应用数据库，不借用 Persona 或内核的事务。
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"xinli/account"
	"xinli/pg"
	"xinli/task"
)

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Receive(ctx context.Context, source task.Source, specs []task.Spec, now time.Time) error {
	if !task.ValidSource(source, specs, now) {
		return task.ErrInvalid
	}
	tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
	if err != nil {
		return err
	}
	defer pg.Rollback(tx)
	if err := receiveTx(ctx, tx, source, specs, now, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func receiveTx(ctx context.Context, tx pgx.Tx, source task.Source, specs []task.Spec, now time.Time, accountChange bool) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM accounts WHERE business_line=$1 AND end_user=$2 FOR UPDATE`, source.BusinessLine, source.EndUser).Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && status != string(account.Active) && !(accountChange && status == string(account.Deleting) && source.ID == account.DeleteOperation) {
		return account.ErrInactive
	}
	result, err := tx.Exec(ctx, `INSERT INTO task_sources(business_line,end_user,source_id,source_version,source_kind,received_at)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, source.BusinessLine, source.EndUser, source.ID, int64(source.Version), source.Kind, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	for _, spec := range specs {
		t := task.New(source, spec, now)
		_, err := tx.Exec(ctx, `INSERT INTO orchestration_tasks(task_id,business_line,end_user,source_id,source_version,source_kind,
			operation,target,business_date,input_id,state,retry_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending',$11)
			ON CONFLICT DO NOTHING`, t.TaskID, source.BusinessLine, source.EndUser, source.ID, int64(source.Version), source.Kind, spec.Operation, spec.Target, spec.Date, t.InputID, now)
		if err != nil {
			return err
		}
	}
	return nil
}

const columns = `task_id,sequence,business_line,end_user,source_id,source_version,source_kind,operation,target,business_date,
	input_id,state,session_id,lease_generation,lease_until,retry_at,attempt,receipt,last_error_class,retirements,command_started_at`

func scan(row pgx.Row) (task.Task, error) {
	var t task.Task
	var version int64
	var until *time.Time
	var started *time.Time
	var receipt []byte
	err := row.Scan(&t.TaskID, &t.Sequence, &t.BusinessLine, &t.EndUser, &t.ID, &version, &t.Kind, &t.Operation, &t.Target, &t.Date,
		&t.InputID, &t.State, &t.Session, &t.LeaseGeneration, &until, &t.RetryAt, &t.Attempt, &receipt, &t.LastErrorClass, &t.Retirements, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, task.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if started != nil {
		t.CommandStartedAt = *started
	}
	t.Version = uint64(version)
	if until != nil {
		t.LeaseUntil = *until
	}
	err = json.Unmarshal(receipt, &t.Receipt)
	return t, err
}

func (s *Store) Get(ctx context.Context, id string) (task.Task, error) {
	return scan(pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT `+columns+` FROM orchestration_tasks WHERE task_id=$1`, id))
}

// 同一逻辑目标按接收顺序执行；前项即使退避或被另一执行者锁住，也不能被后项越过。
const eligible = `NOT EXISTS (SELECT 1 FROM accounts a WHERE a.business_line=t.business_line AND a.end_user=t.end_user AND a.status<>'active' AND t.operation NOT IN ('account_tier','account_delete')) AND t.state IN ('pending','unknown','accepted') AND t.retry_at <= $2
	AND (t.lease_until IS NULL OR t.lease_until <= $2)
	AND NOT EXISTS (SELECT 1 FROM orchestration_tasks p WHERE p.business_line=t.business_line AND p.end_user=t.end_user
		AND p.target=t.target AND p.sequence<t.sequence AND p.state IN ('pending','unknown','accepted'))`

func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, task.ErrInvalid
	}
	rows, err := pg.Executor(ctx, s.pool).Query(ctx, `SELECT t.task_id FROM orchestration_tasks t WHERE `+eligible+` ORDER BY t.sequence LIMIT $1`, limit, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) Claim(ctx context.Context, id string, now time.Time, lease time.Duration) (task.Task, bool, error) {
	if lease <= 0 {
		return task.Task{}, false, task.ErrInvalid
	}
	// 锁和代数递增在同一条语句内完成；网络调用发生在该语句提交以后。
	t, err := scan(pg.Executor(ctx, s.pool).QueryRow(ctx, `UPDATE orchestration_tasks SET lease_generation=lease_generation+1,
		lease_until=$3,attempt=attempt+1 WHERE task_id IN (SELECT t.task_id FROM orchestration_tasks t
		WHERE t.task_id=$1 AND `+eligible+` FOR UPDATE SKIP LOCKED) RETURNING `+columns, id, now, now.Add(lease)))
	if errors.Is(err, task.ErrNotFound) {
		if _, getErr := s.Get(ctx, id); getErr != nil {
			return task.Task{}, false, getErr
		}
		return task.Task{}, false, nil
	}
	return t, err == nil, err
}

func (s *Store) Save(ctx context.Context, next task.Task, now time.Time, release bool) error {
	tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
	if err != nil {
		return err
	}
	defer pg.Rollback(tx)
	old, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM orchestration_tasks WHERE task_id=$1 FOR UPDATE`, next.TaskID))
	if err != nil {
		return err
	}
	if err := task.ValidSave(old, next, now); err != nil {
		return err
	}
	if task.HoldsSession(next) {
		result, err := tx.Exec(ctx, `INSERT INTO task_sessions(business_line,end_user,session_id,task_id) VALUES ($1,$2,$3,$4)
			ON CONFLICT (business_line,end_user,session_id) DO UPDATE SET task_id=EXCLUDED.task_id
			WHERE task_sessions.task_id=EXCLUDED.task_id`, next.BusinessLine, next.EndUser, next.Session, next.TaskID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return task.ErrTargetBusy
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM task_sessions WHERE business_line=$1 AND end_user=$2 AND session_id=$3 AND task_id=$4`, next.BusinessLine, next.EndUser, old.Session, next.TaskID); err != nil {
			return err
		}
	}
	var until *time.Time
	if !release {
		until = &next.LeaseUntil
	}
	var started *time.Time
	if !next.CommandStartedAt.IsZero() {
		started = &next.CommandStartedAt
	}
	receipt, err := json.Marshal(next.Receipt)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE orchestration_tasks SET state=$3,session_id=$4,lease_until=$5,retry_at=$6,receipt=$7,last_error_class=$8,retirements=$10,command_started_at=$11
		WHERE task_id=$1 AND lease_generation=$2 AND lease_until>$9`, next.TaskID, next.LeaseGeneration, next.State, next.Session, until, next.RetryAt, receipt, next.LastErrorClass, now, next.Retirements, started)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return task.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Store) FailureCount(ctx context.Context, owner task.Owner) (int64, error) {
	var n int64
	err := pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT count(*) FROM orchestration_tasks WHERE business_line=$1 AND end_user=$2 AND state='failed'`, owner.BusinessLine, owner.EndUser).Scan(&n)
	return n, err
}

var _ task.Store = (*Store)(nil)
