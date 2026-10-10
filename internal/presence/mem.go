package presence

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Mem 是进程内的 Store，用于单进程部署与测试。
type Mem struct {
	mu       sync.Mutex
	entries  map[string]map[string]*Entry // session → conn → entry
	versions map[string]uint64
	changed  map[string]chan struct{}
}

func NewMem() *Mem {
	return &Mem{entries: map[string]map[string]*Entry{}, versions: map[string]uint64{}, changed: map[string]chan struct{}{}}
}

var _ Store = (*Mem)(nil)

// bump 递增版本并唤醒等待者；调用方持有锁。
func (m *Mem) bump(sid string) {
	m.versions[sid]++
	if ch := m.changed[sid]; ch != nil {
		close(ch)
		delete(m.changed, sid)
	}
}

func (m *Mem) Put(_ context.Context, e *Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *e
	conns := m.entries[e.SessionID]
	if conns == nil {
		conns = map[string]*Entry{}
		m.entries[e.SessionID] = conns
	}
	old := conns[e.ConnID]
	conns[e.ConnID] = &cp
	if old == nil || !old.visible(&cp) {
		m.bump(e.SessionID)
	}
	return nil
}

func (m *Mem) Touch(_ context.Context, connID string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, conns := range m.entries {
		if e := conns[connID]; e != nil {
			e.Expires = expires
		}
	}
	return nil
}

func (m *Mem) Remove(_ context.Context, sessionID, connID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[sessionID][connID]; ok {
		delete(m.entries[sessionID], connID)
		if len(m.entries[sessionID]) == 0 {
			delete(m.entries, sessionID)
		}
		m.bump(sessionID)
	}
	return nil
}

func (m *Mem) List(_ context.Context, sessionID string, now time.Time) ([]*Entry, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Entry
	for _, e := range m.entries[sessionID] {
		if e.Expires.After(now) {
			cp := *e
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *Entry) int { return strings.Compare(a.ConnID, b.ConnID) })
	return out, m.versions[sessionID], nil
}

func (m *Mem) Wait(ctx context.Context, sessionID string, version uint64) error {
	m.mu.Lock()
	if m.versions[sessionID] != version {
		m.mu.Unlock()
		return nil
	}
	ch := m.changed[sessionID]
	if ch == nil {
		ch = make(chan struct{})
		m.changed[sessionID] = ch
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

func (m *Mem) DeleteSession(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 先唤醒等待者，再丢掉版本号：Session 已删除，不会再有新的记录。
	m.bump(sessionID)
	delete(m.entries, sessionID)
	delete(m.versions, sessionID)
	return nil
}

func (m *Mem) Prune(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sid, conns := range m.entries {
		for id, e := range conns {
			if !e.Expires.After(before) {
				delete(conns, id)
			}
		}
		if len(conns) == 0 {
			delete(m.entries, sid)
		}
	}
	return nil
}

// HasSession 报告是否还有该 Session 的记录（含已过期未清理的），供删除不变量检查。
func (m *Mem) HasSession(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries[sessionID]) > 0
}
