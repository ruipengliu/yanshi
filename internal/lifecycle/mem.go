package lifecycle

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemIndex 是 Index 的进程内实现。
type MemIndex struct {
	mu sync.Mutex
	m  map[string]*Session
}

func NewMemIndex() *MemIndex { return &MemIndex{m: map[string]*Session{}} }

var _ Index = (*MemIndex)(nil)

func (x *MemIndex) Put(_ context.Context, s *Session) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.m[s.ID]; !ok {
		cp := *s
		x.m[s.ID] = &cp
	}
	return nil
}

func (x *MemIndex) Touch(_ context.Context, id string, at time.Time) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if s := x.m[id]; s != nil && at.After(s.LastInputAt) {
		s.LastInputAt = at
	}
	return nil
}

func (x *MemIndex) MarkClosed(_ context.Context, id string, at time.Time) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if s := x.m[id]; s != nil && s.ClosedAt.IsZero() {
		s.ClosedAt = at
	}
	return nil
}

func (x *MemIndex) Get(_ context.Context, id string) (*Session, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := x.m[id]
	if s == nil {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

// sorted 返回满足 keep 的 Session，按最后输入时间倒序（同时间按 ID 倒序）。
func (x *MemIndex) sorted(keep func(*Session) bool) []*Session {
	var out []*Session
	for _, s := range x.m {
		if keep(s) {
			cp := *s
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastInputAt.Equal(out[j].LastInputAt) {
			return out[i].LastInputAt.After(out[j].LastInputAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (x *MemIndex) List(_ context.Context, businessLine, endUser, cursor string, limit int) ([]*Session, string, error) {
	var ct time.Time
	var cid string
	if cursor != "" {
		var err error
		if ct, cid, err = DecodeCursor(cursor); err != nil {
			return nil, "", err
		}
	}
	x.mu.Lock()
	all := x.sorted(func(s *Session) bool {
		return s.BusinessLine == businessLine && (endUser == "" || s.EndUser == endUser) && (cursor == "" || before(s, ct, cid))
	})
	x.mu.Unlock()
	if len(all) <= limit {
		return all, "", nil
	}
	return all[:limit], EncodeCursor(all[limit-1]), nil
}

func (x *MemIndex) IDsOf(_ context.Context, businessLine, endUser string) ([]string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	var ids []string
	for _, s := range x.sorted(func(s *Session) bool { return s.BusinessLine == businessLine && s.EndUser == endUser }) {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

func (x *MemIndex) pick(keep func(*Session) bool, limit int) []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var ids []string
	for _, s := range x.m {
		if keep(s) {
			ids = append(ids, s.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

func (x *MemIndex) IdleBefore(_ context.Context, businessLine string, t time.Time, limit int) ([]string, error) {
	return x.pick(func(s *Session) bool {
		return s.BusinessLine == businessLine && s.ClosedAt.IsZero() && s.LastInputAt.Before(t)
	}, limit), nil
}

func (x *MemIndex) ClosedBefore(_ context.Context, businessLine string, t time.Time, limit int) ([]string, error) {
	return x.pick(func(s *Session) bool {
		return s.BusinessLine == businessLine && !s.ClosedAt.IsZero() && s.ClosedAt.Before(t)
	}, limit), nil
}

func (x *MemIndex) Delete(_ context.Context, id string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.m, id)
	return nil
}

// MemDeletions 是 Deletions 的进程内实现。
type MemDeletions struct {
	mu       sync.Mutex
	tombs    map[string]*Tombstone
	requests map[string]*Request
}

func NewMemDeletions() *MemDeletions {
	return &MemDeletions{tombs: map[string]*Tombstone{}, requests: map[string]*Request{}}
}

var _ Deletions = (*MemDeletions)(nil)

func (d *MemDeletions) Mark(_ context.Context, t *Tombstone) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.tombs[t.SessionID]; ok {
		return false, nil
	}
	cp := *t
	d.tombs[t.SessionID] = &cp
	return true, nil
}

func (d *MemDeletions) Get(_ context.Context, id string) (*Tombstone, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tombs[id]
	if t == nil {
		return nil, ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (d *MemDeletions) Complete(_ context.Context, id string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.tombs[id]; t != nil && t.CompletedAt.IsZero() {
		t.CompletedAt = at
	}
	return nil
}

func (d *MemDeletions) Pending(_ context.Context, limit int) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for id, t := range d.tombs {
		if t.CompletedAt.IsZero() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (d *MemDeletions) CreateRequest(_ context.Context, r *Request) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.requests[r.ID]; !ok {
		cp := *r
		d.requests[r.ID] = &cp
	}
	return nil
}

func (d *MemDeletions) Request(_ context.Context, id string) (*Request, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.requests[id]
	if r == nil {
		return nil, ErrNotFound
	}
	cp := *r
	cp.Sessions, cp.Completed = 0, 0
	for _, t := range d.tombs {
		if t.RequestID == id {
			cp.Sessions++
			if !t.CompletedAt.IsZero() {
				cp.Completed++
			}
		}
	}
	return &cp, nil
}
