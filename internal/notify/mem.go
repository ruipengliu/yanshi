package notify

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// MemRegistry 是进程内的 Registry，用于单进程部署与测试。
type MemRegistry struct {
	mu      sync.Mutex
	devices map[string]*Device // business_line/end_user/device_id
}

func NewMemRegistry() *MemRegistry { return &MemRegistry{devices: map[string]*Device{}} }

var _ Registry = (*MemRegistry)(nil)

func key(bl, eu, dev string) string { return bl + "\x00" + eu + "\x00" + dev }

func (m *MemRegistry) Register(_ context.Context, d *Device, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(d.BusinessLine, d.EndUser, d.DeviceID)
	cp := *d
	cp.LastActive = at
	if old := m.devices[k]; old != nil {
		cp.LastActive = old.LastActive
	}
	m.devices[k] = &cp
	return nil
}

func (m *MemRegistry) Touch(_ context.Context, bl, eu, dev string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d := m.devices[key(bl, eu, dev)]; d != nil && at.After(d.LastActive) {
		d.LastActive = at
	}
	return nil
}

func (m *MemRegistry) List(_ context.Context, bl, eu string) ([]*Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Device
	for _, d := range m.devices {
		if d.BusinessLine == bl && d.EndUser == eu {
			cp := *d
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, byRecency)
	return out, nil
}

// byRecency 按 LastActive 从近到远，相同时按 DeviceID。
func byRecency(a, b *Device) int {
	if c := b.LastActive.Compare(a.LastActive); c != 0 {
		return c
	}
	return strings.Compare(a.DeviceID, b.DeviceID)
}

func (m *MemRegistry) Unregister(_ context.Context, bl, eu, dev string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.devices, key(bl, eu, dev))
	return nil
}

func (m *MemRegistry) DeleteEndUser(_ context.Context, bl, eu string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, d := range m.devices {
		if d.BusinessLine == bl && d.EndUser == eu {
			delete(m.devices, k)
		}
	}
	return nil
}
