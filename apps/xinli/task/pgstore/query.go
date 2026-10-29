package pgstore

import (
	"context"

	"xinli/pg"
	"xinli/task"
)

func (s *Store) List(ctx context.Context, owner task.Owner, after int64, limit int) ([]task.Task, error) {
	if owner.BusinessLine == "" || owner.EndUser == "" || after < 0 || limit < 1 || limit > 101 {
		return nil, task.ErrInvalid
	}
	rows, err := pg.Executor(ctx, s.pool).Query(ctx, `SELECT `+columns+` FROM orchestration_tasks
		WHERE business_line=$1 AND end_user=$2 AND sequence>$3 ORDER BY sequence LIMIT $4`, owner.BusinessLine, owner.EndUser, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []task.Task{}
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Read(ctx context.Context, owner task.Owner, id string) (task.Task, error) {
	return scan(pg.Executor(ctx, s.pool).QueryRow(ctx, `SELECT `+columns+` FROM orchestration_tasks
		WHERE business_line=$1 AND end_user=$2 AND task_id=$3`, owner.BusinessLine, owner.EndUser, id))
}

var _ task.Reader = (*Store)(nil)
