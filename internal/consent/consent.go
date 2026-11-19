// Package consent 保存业务线登记的 EndUser 单独同意（ADR-0022 修订）：处理某类敏感个人信息之前，业务线在自己的产品中
// 取得用户同意，再经 API 登记；撤回时删除登记。目前只有健康信息一类。
package consent

import (
	"context"
	"sync"
	"time"

	"yanshi/internal/clock"
)

// Kind 是需要单独同意的信息类别。
type Kind string

// Health 是健康信息（Memory 的 health 类别，ADR-0022）。
const Health Kind = "health"

// Valid 报告 k 是否是平台认识的类别；为将来的敏感类别留位置，目前只有 Health。
func Valid(k Kind) bool { return k == Health }

// Store 保存同意记录；实现须通过 consenttest 一致性套件。每次调用只涉及一个 EndUser。
// 没有记录时 Get 返回零时间；Put 已有记录时保留原同意时间（重试与重复登记幂等）；删除幂等。
type Store interface {
	Get(ctx context.Context, businessLine, endUser string, kind Kind) (time.Time, error)
	Put(ctx context.Context, businessLine, endUser string, kind Kind, at time.Time) error
	Delete(ctx context.Context, businessLine, endUser string, kind Kind) error
	// DeleteEndUser 删除 EndUser 在业务线中的全部同意记录（注销账号）。
	DeleteEndUser(ctx context.Context, businessLine, endUser string) error
}

type key struct {
	businessLine, endUser string
	kind                  Kind
}

type Mem struct {
	mu   sync.Mutex
	rows map[key]time.Time
}

func NewMem() *Mem { return &Mem{rows: map[key]time.Time{}} }

func (m *Mem) Get(_ context.Context, bl, eu string, kind Kind) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[key{bl, eu, kind}], nil
}

func (m *Mem) Put(_ context.Context, bl, eu string, kind Kind, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[key{bl, eu, kind}]; !ok {
		m.rows[key{bl, eu, kind}] = at
	}
	return nil
}

func (m *Mem) Delete(_ context.Context, bl, eu string, kind Kind) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, key{bl, eu, kind})
	return nil
}

func (m *Mem) DeleteEndUser(_ context.Context, bl, eu string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.rows {
		if k.businessLine == bl && k.endUser == eu {
			delete(m.rows, k)
		}
	}
	return nil
}

// CacheTTL 与档位缓存（usage.TierCacheTTL）相同：开启与撤回跨进程至多 5 秒生效。
const CacheTTL = 5 * time.Second

// Cache 缓存同意记录的读。本进程的写入与删除立即失效对应条目，撤回在本进程立即生效；做法同 usage.TierCache：
// 全局世代使失效前发起的读既不回填，也不把旧值交给调用方。
type Cache struct {
	Store Store
	Clock clock.Clock

	mu         sync.Mutex
	generation uint64
	entries    map[key]entry
}

type entry struct {
	granted time.Time
	at      time.Time
}

var _ Store = (*Cache)(nil)

func NewCache(s Store, c clock.Clock) *Cache {
	return &Cache{Store: s, Clock: c, entries: map[key]entry{}}
}

func (c *Cache) Get(ctx context.Context, bl, eu string, kind Kind) (time.Time, error) {
	k := key{bl, eu, kind}
	for {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		now := c.Clock.Now()
		c.mu.Lock()
		if e, ok := c.entries[k]; ok && now.Sub(e.at) < CacheTTL {
			c.mu.Unlock()
			return e.granted, nil
		}
		generation := c.generation
		c.mu.Unlock()
		granted, err := c.Store.Get(ctx, bl, eu, kind)
		if err != nil {
			return time.Time{}, err
		}
		c.mu.Lock()
		if generation != c.generation {
			c.mu.Unlock()
			continue // 失效前发起的读可能看到撤回之前的同意。
		}
		for key, e := range c.entries {
			if now.Sub(e.at) >= CacheTTL {
				delete(c.entries, key)
			}
		}
		c.entries[k] = entry{granted: granted, at: now}
		c.mu.Unlock()
		return granted, nil
	}
}

// Invalidate 失效 EndUser 在业务线中全部类别的缓存；也供 Janitor 在存储事务结束后调用，防止提交前的读回填旧值。
func (c *Cache) Invalidate(bl, eu string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	for k := range c.entries {
		if k.businessLine == bl && k.endUser == eu {
			delete(c.entries, k)
		}
	}
}

func (c *Cache) Put(ctx context.Context, bl, eu string, kind Kind, at time.Time) error {
	defer c.Invalidate(bl, eu) // 报错也可能已提交。
	return c.Store.Put(ctx, bl, eu, kind, at)
}

func (c *Cache) Delete(ctx context.Context, bl, eu string, kind Kind) error {
	defer c.Invalidate(bl, eu)
	return c.Store.Delete(ctx, bl, eu, kind)
}

func (c *Cache) DeleteEndUser(ctx context.Context, bl, eu string) error {
	defer c.Invalidate(bl, eu)
	return c.Store.DeleteEndUser(ctx, bl, eu)
}
