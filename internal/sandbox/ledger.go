package sandbox

import (
	"fmt"
	"sync"
	"time"

	"yanshi/sdk/nodesdk"
)

// Ledger 用真实 Session 隔离外部 call_id；槽位与 call_id 的外形都不能代替归属键。
type Ledger interface {
	Get(sessionID, callID string) (*nodesdk.Record, error)
	Put(sessionID, callID string, r *nodesdk.Record) error
	Forget(sessionID string) error
	Prune(before time.Time) error
	// HasSession 包含过期未清理的记录，供删除不变量在内存与 PostgreSQL 上使用。
	HasSession(sessionID string) (bool, error)
}

// BindLedger 为一个沙箱的 Executor 固定 Session，保留三端 SDK 的账本契约。
func BindLedger(l Ledger, sessionID string) nodesdk.Ledger {
	return boundLedger{Ledger: l, sessionID: sessionID}
}

type boundLedger struct {
	Ledger
	sessionID string
}

func (l boundLedger) Get(callID string) (*nodesdk.Record, error) {
	return l.Ledger.Get(l.sessionID, callID)
}

func (l boundLedger) Put(callID string, r *nodesdk.Record) error {
	return l.Ledger.Put(l.sessionID, callID, r)
}

func (l boundLedger) Forget(sessionID string) error {
	if sessionID != l.sessionID {
		return fmt.Errorf("sandbox ledger: session mismatch")
	}
	return l.Ledger.Forget(sessionID)
}

// MemLedger 每个 Session 共用一份 SDK 内存账本，控制器接管时仍能读到 started。
type MemLedger struct {
	mu       sync.Mutex
	sessions map[string]*nodesdk.MemLedger
}

func NewMemLedger() *MemLedger {
	return &MemLedger{sessions: map[string]*nodesdk.MemLedger{}}
}

var _ Ledger = (*MemLedger)(nil)

func (m *MemLedger) Get(sessionID, callID string) (*nodesdk.Record, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("sandbox ledger: Session is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.sessions[sessionID]; l != nil {
		return l.Get(callID)
	}
	return &nodesdk.Record{}, nil
}

func (m *MemLedger) Put(sessionID, callID string, r *nodesdk.Record) error {
	if sessionID == "" || r.SessionID != sessionID {
		return fmt.Errorf("sandbox ledger: session mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.sessions[sessionID]
	if l == nil {
		l = nodesdk.NewMemLedger()
		m.sessions[sessionID] = l
	}
	cp := *r
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now()
	}
	return l.Put(callID, &cp)
}

func (m *MemLedger) Forget(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
	return nil
}

func (m *MemLedger) Prune(before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sid, l := range m.sessions {
		if err := l.Prune(before); err != nil {
			return err
		}
		if !l.HasSession(sid) {
			delete(m.sessions, sid)
		}
	}
	return nil
}

func (m *MemLedger) HasSession(sessionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.sessions[sessionID]
	return l != nil && l.HasSession(sessionID), nil
}
