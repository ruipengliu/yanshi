package node

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
)

// MemDirectory 是 Directory 的进程内实现。
type MemDirectory struct {
	Clock clock.Clock
	mu    sync.Mutex
	nodes map[string]*Info
	gens  map[string]uint64
}

func NewMemDirectory(c clock.Clock) *MemDirectory {
	return &MemDirectory{Clock: c, nodes: map[string]*Info{}, gens: map[string]uint64{}}
}

func (d *MemDirectory) Register(_ context.Context, info Info) (string, uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if old := d.nodes[info.NodeID]; old != nil && old.Scope != info.Scope {
		return "", 0, fmt.Errorf("node %s belongs to another end user", info.NodeID)
	}
	base := SanitizeLabel(info.Label)
	label := base
	for n := 2; d.labelTaken(info.Scope, label, info.NodeID); n++ {
		label = fmt.Sprintf("%s-%d", base, n)
	}
	cp := info
	cp.Label, cp.Online, cp.LastSeen = label, true, d.Clock.Now()
	cp.Capabilities = slices.Clone(info.Capabilities)
	d.nodes[info.NodeID] = &cp
	d.gens[info.NodeID]++
	return label, d.gens[info.NodeID], nil
}

func (d *MemDirectory) labelTaken(scope Scope, label, self string) bool {
	for id, n := range d.nodes {
		if id != self && n.Scope == scope && n.Label == label {
			return true
		}
	}
	return false
}

func (d *MemDirectory) SetOffline(_ context.Context, nodeID string, gen uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.nodes[nodeID]
	if n == nil {
		return ErrNotFound
	}
	if d.gens[nodeID] == gen {
		n.Online, n.LastSeen = false, d.Clock.Now()
	}
	return nil
}

func (d *MemDirectory) Get(_ context.Context, nodeID string) (*Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.nodes[nodeID]
	if n == nil {
		return nil, ErrNotFound
	}
	cp := *n
	return &cp, nil
}

func (d *MemDirectory) List(_ context.Context, scope Scope) ([]*Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*Info
	for _, n := range d.nodes {
		if n.Scope == scope {
			cp := *n
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// MemInbox 是 Inbox 的进程内实现。
type MemInbox struct {
	mu    sync.Mutex
	boxes map[string]*box
}

type box struct {
	items   []*v1.Invoke
	version uint64
	changed chan struct{}
}

func NewMemInbox() *MemInbox { return &MemInbox{boxes: map[string]*box{}} }

func (m *MemInbox) box(id string) *box {
	b := m.boxes[id]
	if b == nil {
		b = &box{changed: make(chan struct{})}
		m.boxes[id] = b
	}
	return b
}

func (b *box) bump() {
	b.version++
	close(b.changed)
	b.changed = make(chan struct{})
}

func (m *MemInbox) Put(_ context.Context, nodeID string, inv *v1.Invoke) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.box(nodeID)
	for _, it := range b.items {
		if it.GetCallId() == inv.GetCallId() {
			return nil
		}
	}
	b.items = append(b.items, proto.Clone(inv).(*v1.Invoke))
	b.bump()
	return nil
}

func (m *MemInbox) Remove(_ context.Context, nodeID, callID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.box(nodeID)
	for i, it := range b.items {
		if it.GetCallId() == callID {
			b.items = append(b.items[:i], b.items[i+1:]...)
			b.bump()
			return nil
		}
	}
	return nil
}

func (m *MemInbox) Pending(_ context.Context, nodeID string) ([]*v1.Invoke, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.box(nodeID)
	return slices.Clone(b.items), b.version, nil
}

func (m *MemInbox) Wait(ctx context.Context, nodeID string, version uint64) error {
	m.mu.Lock()
	b := m.box(nodeID)
	if b.version != version {
		m.mu.Unlock()
		return nil
	}
	ch := b.changed
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}
