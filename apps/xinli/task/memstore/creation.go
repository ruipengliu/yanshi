package memstore

import (
	"context"
	"sort"
	"time"

	"xinli/task"
)

func (s *Store) BeginCreation(ctx context.Context, t task.Task, c task.Creation, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	old, ok := s.tasks[t.TaskID]
	if !ok {
		return task.ErrNotFound
	}
	if err := task.ValidSave(old, t, now); err != nil {
		return err
	}
	if !task.ValidCreation(t, c, now) {
		return task.ErrInvalid
	}
	if _, exists := s.creations[c.ID]; exists {
		return task.ErrInvalid
	}
	s.creations[c.ID] = c
	return nil
}

func (s *Store) RecordCreated(ctx context.Context, id, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c, ok := s.creations[id]
	if !ok {
		return task.ErrInvalid
	}
	if session == "" || c.Session != "" && c.Session != session {
		return task.ErrInvalid
	}
	c.Session = session
	s.creations[id] = c
	return nil
}

func (s *Store) Creations(ctx context.Context, before time.Time, limit int) ([]task.Creation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, task.ErrInvalid
	}
	out := []task.Creation{}
	for _, c := range s.creations {
		if !maxTime(c.CreatedAt, c.CheckedAt).After(before) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if maxTime(out[i].CreatedAt, out[i].CheckedAt).Equal(maxTime(out[j].CreatedAt, out[j].CheckedAt)) {
			return out[i].ID < out[j].ID
		}
		return maxTime(out[i].CreatedAt, out[i].CheckedAt).Before(maxTime(out[j].CreatedAt, out[j].CheckedAt))
	})
	return out[:min(limit, len(out))], nil
}

func (s *Store) ForgetCreation(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(s.creations, id)
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func (s *Store) DeferCreation(ctx context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c, ok := s.creations[id]
	if !ok {
		return nil
	}
	c.CheckedAt = now
	s.creations[id] = c
	return nil
}

func (s *Store) SessionTask(ctx context.Context, owner task.Owner, session string) (task.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Task{}, false, err
	}
	for _, t := range s.tasks {
		if t.Owner == owner && t.Session == session && task.HoldsSession(t) {
			return t, true, nil
		}
	}
	return task.Task{}, false, nil
}
