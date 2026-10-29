package memstore

import (
	"context"
	"slices"

	"xinli/task"
)

func (s *Store) List(ctx context.Context, owner task.Owner, after int64, limit int) ([]task.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner.BusinessLine == "" || owner.EndUser == "" || after < 0 || limit < 1 || limit > 101 {
		return nil, task.ErrInvalid
	}
	out := []task.Task{}
	for _, item := range s.tasks {
		if item.Owner == owner && item.Sequence > after {
			out = append(out, item)
		}
	}
	slices.SortFunc(out, func(a, b task.Task) int {
		if a.Sequence < b.Sequence {
			return -1
		}
		if a.Sequence > b.Sequence {
			return 1
		}
		return 0
	})
	return out[:min(limit, len(out))], nil
}

func (s *Store) Read(ctx context.Context, owner task.Owner, id string) (task.Task, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return task.Task{}, err
	}
	if t.Owner != owner {
		return task.Task{}, task.ErrNotFound
	}
	return t, nil
}

var _ task.Reader = (*Store)(nil)
