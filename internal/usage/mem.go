package usage

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// Mem 是内存中的 Store，用于测试、模拟与单进程开发。
type Mem struct {
	mu      sync.Mutex
	entries map[string]*Entry
	byKey   map[string]string
}

func NewMem() *Mem { return &Mem{entries: map[string]*Entry{}, byKey: map[string]string{}} }

var _ Store = (*Mem)(nil)

func (m *Mem) Record(_ context.Context, e *Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[e.ID]; ok {
		return nil
	}
	if _, ok := m.byKey[e.DedupKey()]; ok {
		return nil
	}
	cp := *e
	cp.At = cp.At.UTC()
	m.entries[e.ID] = &cp
	m.byKey[e.DedupKey()] = e.ID
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
		case ByOneshot:
			key = e.Oneshot
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
		r.CachedInputTokens += e.CachedInputTokens
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

func (m *Mem) AnonymizeEntry(_ context.Context, endUser, id string) error {
	if endUser == "" || endUser == Anonymous {
		return errors.New("usage: invalid end user")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.EndUser == endUser && (e.ID == id || e.DedupKey() == id) {
			e.EndUser = Anonymous
		}
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
			delete(m.byKey, e.DedupKey())
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

// Entries 返回副本供模拟差分检查持久的对象 ID、幂等键与匿名化结果。
func (m *Mem) Entries() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })
	return out
}
