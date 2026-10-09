package memory

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"
)

// MemStore 是 Store 的进程内实现。
type MemStore struct {
	mu       sync.Mutex
	m        map[string]*Memory
	vecs     map[string]Vector
	accesses []Access
}

func NewMemStore() *MemStore { return &MemStore{m: map[string]*Memory{}, vecs: map[string]Vector{}} }

var _ Store = (*MemStore)(nil)

func (s *MemStore) Put(_ context.Context, m *Memory, vec *Vector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *m
	s.m[m.ID] = &cp
	delete(s.vecs, m.ID)
	if vec != nil {
		s.vecs[m.ID] = Vector{Model: vec.Model, Values: slices.Clone(vec.Values)}
	}
	return nil
}

func (s *MemStore) Get(_ context.Context, id string) (*Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m[id]
	if m == nil {
		return nil, ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *MemStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remove(id)
	return nil
}

// remove 删除 Memory 及其向量与访问记录；调用方持有锁。
func (s *MemStore) remove(id string) {
	delete(s.m, id)
	delete(s.vecs, id)
	s.accesses = slices.DeleteFunc(s.accesses, func(a Access) bool { return a.MemoryID == id })
}

func (s *MemStore) Count(_ context.Context, businessLine, endUser string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.m {
		if m.BusinessLine == businessLine && m.EndUser == endUser {
			n++
		}
	}
	return n, nil
}

func (s *MemStore) inScope(endUser string, scopes []Scope) []*Memory {
	var out []*Memory
	for _, m := range s.m {
		if m.EndUser != endUser {
			continue
		}
		for _, sc := range scopes {
			if sc.allows(m) {
				cp := *m
				out = append(out, &cp)
				break
			}
		}
	}
	return out
}

func (s *MemStore) List(_ context.Context, endUser string, scopes []Scope, limit int) ([]*Memory, error) {
	s.mu.Lock()
	out := s.inScope(endUser, scopes)
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemStore) Nearest(_ context.Context, endUser string, scopes []Scope, vec Vector, limit int) ([]Hit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hits []Hit
	for _, m := range s.inScope(endUser, scopes) {
		v, ok := s.vecs[m.ID]
		if !ok || v.Model != vec.Model {
			continue
		}
		hits = append(hits, Hit{Memory: m, Score: Cosine(v.Values, vec.Values)})
	}
	return rank(hits, limit), nil
}

func (s *MemStore) deleteWhere(keep func(*Memory) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, m := range s.m {
		if !keep(m) {
			s.remove(id)
		}
	}
}

func (s *MemStore) DeleteSession(_ context.Context, sessionID string) error {
	s.deleteWhere(func(m *Memory) bool { return m.SourceSession != sessionID })
	return nil
}

func (s *MemStore) DeleteEndUser(_ context.Context, businessLine, endUser string) error {
	s.deleteWhere(func(m *Memory) bool { return m.BusinessLine != businessLine || m.EndUser != endUser })
	return nil
}

// HasSession 报告是否仍有由该 Session 写入的 Memory（模拟测试检查删除是否彻底）。
func (s *MemStore) HasSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.m {
		if m.SourceSession == sessionID {
			return true
		}
	}
	return false
}

// MemGrants 是 Grants 的进程内实现。
type MemGrants struct {
	mu sync.Mutex
	m  map[string]*Grant
}

func NewMemGrants() *MemGrants { return &MemGrants{m: map[string]*Grant{}} }

var _ Grants = (*MemGrants)(nil)

func cloneGrant(g *Grant) *Grant {
	cp := *g
	cp.Categories = slices.Clone(g.Categories)
	return &cp
}

func (s *MemGrants) Create(_ context.Context, g *Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[g.ID]; !ok {
		s.m[g.ID] = cloneGrant(g)
	}
	return nil
}

func (s *MemGrants) Get(_ context.Context, id string) (*Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.m[id]
	if g == nil {
		return nil, ErrNotFound
	}
	return cloneGrant(g), nil
}

func (s *MemGrants) Revoke(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.m[id]; g != nil && g.RevokedAt.IsZero() {
		g.RevokedAt = at
	}
	return nil
}

func (s *MemGrants) filter(keep func(*Grant) bool) []*Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Grant
	for _, g := range s.m {
		if keep(g) {
			out = append(out, cloneGrant(g))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *MemGrants) List(_ context.Context, endUser, businessLine string) ([]*Grant, error) {
	return s.filter(func(g *Grant) bool {
		return g.EndUser == endUser && (g.From == businessLine || g.To == businessLine)
	}), nil
}

func (s *MemGrants) ToReader(_ context.Context, endUser, to string) ([]*Grant, error) {
	return s.filter(func(g *Grant) bool { return g.EndUser == endUser && g.To == to }), nil
}

func (s *MemGrants) DeleteBusinessLine(_ context.Context, endUser, businessLine string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, g := range s.m {
		if g.EndUser == endUser && (g.From == businessLine || g.To == businessLine) {
			delete(s.m, id)
		}
	}
	return nil
}

func (s *MemStore) RecordAccess(_ context.Context, accesses []Access) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range accesses {
		if _, ok := s.m[a.MemoryID]; ok {
			s.accesses = append(s.accesses, a)
		}
	}
	return nil
}

func (s *MemStore) Accesses(_ context.Context, endUser, owner string, since time.Time, limit int) ([]Access, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Access
	for _, a := range s.accesses {
		if a.EndUser == endUser && a.Owner == owner && !a.At.Before(since) {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
