package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"xinli/pg"
	"xinli/task"
)

func (s *Store) BeginCreation(ctx context.Context, t task.Task, c task.Creation, now time.Time) error {
	tx, err := pg.Executor(ctx, s.pool).Begin(ctx)
	if err != nil {
		return err
	}
	defer pg.Rollback(tx)
	old, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM orchestration_tasks WHERE task_id=$1 FOR UPDATE`, t.TaskID))
	if err != nil {
		return err
	}
	if err := task.ValidSave(old, t, now); err != nil {
		return err
	}
	if !task.ValidCreation(t, c, now) {
		return task.ErrInvalid
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO session_creations(id,created_at,scan_at,payload,business_line,end_user,task_id) VALUES ($1,$2,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, c.ID, c.CreatedAt, payload, c.BusinessLine, c.EndUser, c.TaskID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return task.ErrInvalid
	}
	return tx.Commit(ctx)
}

func (s *Store) RecordCreated(ctx context.Context, id, session string) error {
	if session == "" {
		return task.ErrInvalid
	}
	result, err := pg.Executor(ctx, s.pool).Exec(ctx, `UPDATE session_creations SET payload=jsonb_set(payload,'{Session}',to_jsonb($2::text))
		WHERE id=$1 AND (payload->>'Session'='' OR payload->>'Session'=$2)`, id, session)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return task.ErrInvalid
	}
	return nil
}

func (s *Store) Creations(ctx context.Context, before time.Time, limit int) ([]task.Creation, error) {
	if limit <= 0 {
		return nil, task.ErrInvalid
	}
	rows, err := pg.Executor(ctx, s.pool).Query(ctx, `SELECT payload FROM session_creations WHERE scan_at<=$1 ORDER BY scan_at,id LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []task.Creation{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var c task.Creation
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ForgetCreation(ctx context.Context, id string) error {
	_, err := pg.Executor(ctx, s.pool).Exec(ctx, `DELETE FROM session_creations WHERE id=$1`, id)
	return err
}

func (s *Store) DeferCreation(ctx context.Context, id string, now time.Time) error {
	_, err := pg.Executor(ctx, s.pool).Exec(ctx, `UPDATE session_creations SET scan_at=$2,payload=jsonb_set(payload,'{CheckedAt}',to_jsonb($2::timestamptz)) WHERE id=$1`, id, now)
	return err
}

func (s *Store) SessionTask(ctx context.Context, owner task.Owner, session string) (task.Task, bool, error) {
	t, err := scan(pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT `+columns+` FROM orchestration_tasks WHERE task_id=(
		SELECT task_id FROM task_sessions WHERE business_line=$1 AND end_user=$2 AND session_id=$3)`, owner.BusinessLine, owner.EndUser, session))
	if errors.Is(err, task.ErrNotFound) {
		return task.Task{}, false, nil
	}
	return t, err == nil, err
}
