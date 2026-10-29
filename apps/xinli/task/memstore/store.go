package memstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"xinli/task"
)

type sourceKey struct {
	task.Owner
	ID      string
	Version uint64
}
type Store struct {
	mu       sync.Mutex
	sources  map[sourceKey]bool
	tasks    map[string]task.Task
	sequence int64
}

func New() *Store {
	return &Store{sources: make(map[sourceKey]bool), tasks: make(map[string]task.Task)}
}

func (s *Store) Receive(ctx context.Context, source task.Source, specs []task.Spec, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !task.ValidSource(source, specs, now) {
		return task.ErrInvalid
	}
	key := sourceKey{source.Owner, source.ID, source.Version}
	if s.sources[key] {
		return nil
	}
	for _, spec := range specs {
		t := task.New(source, spec, now)
		if _, exists := s.tasks[t.TaskID]; exists {
			continue
		}
		s.sequence++
		t.Sequence = s.sequence
		s.tasks[t.TaskID] = t
	}
	s.sources[key] = true
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (task.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	t, ok := s.tasks[id]
	if !ok {
		return task.Task{}, task.ErrNotFound
	}
	return t, nil
}

func (s *Store) due(t task.Task, now time.Time) bool {
	if !task.Ready(t, now) {
		return false
	}
	for _, other := range s.tasks {
		if other.Owner == t.Owner && other.Target == t.Target && other.Sequence < t.Sequence && !other.Terminal() {
			return false
		}
	}
	return true
}

func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, task.ErrInvalid
	}
	list := []task.Task{}
	for _, t := range s.tasks {
		if s.due(t, now) {
			list = append(list, t)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Sequence < list[j].Sequence })
	ids := []string{}
	for _, t := range list[:min(limit, len(list))] {
		ids = append(ids, t.TaskID)
	}
	return ids, nil
}

func (s *Store) Claim(ctx context.Context, id string, now time.Time, lease time.Duration) (task.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Task{}, false, err
	}
	if lease <= 0 {
		return task.Task{}, false, task.ErrInvalid
	}
	t, ok := s.tasks[id]
	if !ok {
		return task.Task{}, false, task.ErrNotFound
	}
	if !s.due(t, now) {
		return task.Task{}, false, nil
	}
	t.LeaseGeneration++
	t.Attempt++
	t.LeaseUntil = now.Add(lease)
	s.tasks[id] = t
	return t, true, nil
}

func (s *Store) Save(ctx context.Context, next task.Task, now time.Time, release bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	old, ok := s.tasks[next.TaskID]
	if !ok {
		return task.ErrNotFound
	}
	if err := task.ValidSave(old, next, now); err != nil {
		return err
	}
	if task.HoldsSession(next) {
		for _, other := range s.tasks {
			if other.TaskID != next.TaskID && other.Owner == next.Owner && other.Session == next.Session && task.HoldsSession(other) {
				return task.ErrTargetBusy
			}
		}
	}
	if release {
		next.LeaseUntil = time.Time{}
	}
	s.tasks[next.TaskID] = next
	return nil
}

func (s *Store) FailureCount(ctx context.Context, owner task.Owner) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, t := range s.tasks {
		if t.Owner == owner && t.State == task.Failed {
			n++
		}
	}
	return n, nil
}

var _ task.Store = (*Store)(nil)
