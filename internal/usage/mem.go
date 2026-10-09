package usage

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// Mem 是内存中的 Store，用于测试、模拟与单进程开发。
type Mem struct {
	mu      sync.Mutex
	entries map[string]*Entry
}

func NewMem() *Mem { return &Mem{entries: map[string]*Entry{}} }

var _ Store = (*Mem)(nil)

func (m *Mem) Record(_ context.Context, e *Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[e.ID]; !ok {
		cp := *e
		cp.At = cp.At.UTC()
		m.entries[e.ID] = &cp
	}
	return nil
}

// inHours 与按小时聚合的实现保持一致：按记录所在的整点判断是否落在 [from, to)。
func inHours(at, from, to time.Time) bool {
	h := at.Truncate(time.Hour)
	return !h.Before(from) && h.Before(to)
}

func (m *Mem) Spent(_ context.Context, businessLine, endUser string, from, to time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sum int64
	for _, e := range m.entries {
		if e.BusinessLine == businessLine && (endUser == "" || e.EndUser == endUser) && inHours(e.At, from, to) {
			sum += e.Cost
		}
	}
	return sum, nil
}

func (m *Mem) Report(_ context.Context, q Query) ([]Row, error) {
	loc := q.Location
	if loc == nil {
		loc = time.UTC
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := map[string]*Row{}
	for _, e := range m.entries {
		if e.BusinessLine != q.BusinessLine || (q.EndUser != "" && e.EndUser != q.EndUser) || e.At.Before(q.From) || !e.At.Before(q.To) {
			continue
		}
		var key string
		switch q.GroupBy {
		case ByDay:
			key = e.At.In(loc).Format(time.DateOnly)
		case ByEndUser:
			key = e.EndUser
		case ByModel:
			key = e.Model
		case ByAgent:
			key = e.Agent
		default:
			return nil, errors.New("usage: unknown group_by")
		}
		r := rows[key]
		if r == nil {
			r = &Row{Key: key}
			rows[key] = r
		}
		r.Calls++
		r.Cost += e.Cost
		r.InputTokens += e.InputTokens
		r.OutputTokens += e.OutputTokens
		r.SandboxMillis += e.SandboxMillis
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Row) int {
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	return out, nil
}

func (m *Mem) DeleteEndUser(_ context.Context, businessLine, endUser string) error {
	if endUser == "" || endUser == Anonymous {
		return errors.New("usage: invalid end user")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.BusinessLine == businessLine && e.EndUser == endUser {
			e.EndUser = Anonymous
		}
	}
	return nil
}

func (m *Mem) AnonymizeEntry(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[id]; ok {
		e.EndUser = Anonymous
	}
	return nil
}

func (m *Mem) Prune(_ context.Context, before time.Time) error {
	before = before.Truncate(time.Hour)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.entries {
		if e.At.Truncate(time.Hour).Before(before) {
			delete(m.entries, id)
		}
	}
	return nil
}

// EndUsers 返回出现在用量中的全部 (业务线, EndUser)，供模拟测试检查注销后的匿名化。
func (m *Mem) EndUsers() map[[2]string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[[2]string]bool{}
	for _, e := range m.entries {
		out[[2]string{e.BusinessLine, e.EndUser}] = true
	}
	return out
}
