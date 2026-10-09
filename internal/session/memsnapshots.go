package session

import (
	"context"
	"sync"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

// MemSnapshots 是 Snapshots 的进程内实现。存放序列化后的副本，与持久化实现一样不与调用方共享对象。
type MemSnapshots struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewMemSnapshots() *MemSnapshots { return &MemSnapshots{m: map[string][]byte{}} }

var _ Snapshots = (*MemSnapshots)(nil)

func (s *MemSnapshots) Get(_ context.Context, id string) (*v1.SessionSnapshot, error) {
	s.mu.Lock()
	b, ok := s.m[id]
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}
	snap := &v1.SessionSnapshot{}
	return snap, proto.Unmarshal(b, snap)
}

func (s *MemSnapshots) Put(_ context.Context, snap *v1.SessionSnapshot) error {
	b, err := proto.Marshal(snap)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.m[snap.GetSessionId()]; ok {
		prev := &v1.SessionSnapshot{}
		if proto.Unmarshal(old, prev) == nil && prev.GetSeq() >= snap.GetSeq() {
			return nil
		}
	}
	s.m[snap.GetSessionId()] = b
	return nil
}

func (s *MemSnapshots) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

// Has 报告是否存有该 Session 的快照（模拟测试检查删除是否彻底）。
func (s *MemSnapshots) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[id]
	return ok
}
