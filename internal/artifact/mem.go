package artifact

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"

	"yanshi/internal/shard"
)

// MemMeta 是 MetaStore 的进程内实现。
type MemMeta struct {
	// Locator 只校验唯一目标，装配层已选定本存储。
	Locator *shard.Locator
	mu      sync.Mutex
	m       map[string]*Meta
}

func NewMemMeta() *MemMeta { return &MemMeta{m: map[string]*Meta{}} }

func (s *MemMeta) Create(_ context.Context, m *Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *m
	s.m[m.ID] = &cp
	return nil
}

func (s *MemMeta) Get(_ context.Context, sessionID, id string) (*Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[id]
	if !ok || m.SessionID != sessionID {
		return nil, ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *MemMeta) List(_ context.Context, sessionID string) ([]*Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Meta
	for _, m := range s.m {
		if m.SessionID == sessionID {
			cp := *m
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// MemBlobs 是 BlobStore 的进程内实现，用于测试与模拟。
type MemBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewMemBlobs() *MemBlobs { return &MemBlobs{m: map[string][]byte{}} }

func (s *MemBlobs) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = b
	return nil
}

func (s *MemBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *MemBlobs) Copy(_ context.Context, src, dst string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[src]
	if !ok {
		return ErrNotFound
	}
	s.m[dst] = bytes.Clone(b)
	return nil
}

func (s *MemBlobs) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func (s *MemMeta) Delete(_ context.Context, sessionID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.m[id]; m != nil && m.SessionID == sessionID {
		delete(s.m, id)
	}
	return nil
}

func (s *MemMeta) Lookup(_ context.Context, id string) (*Meta, error) {
	locator := shard.DefaultLocator()
	if s.Locator != nil {
		locator = *s.Locator
	}
	if _, err := locator.Locate(shard.Target{Kind: shard.Object, Key: id}); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *m
	return &cp, nil
}
