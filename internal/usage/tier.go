package usage

import (
	"context"
	"sync"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/metrics"
)

// TierStore 保存业务线分配的档位，不保存权益；实现须通过 tiertest 一致性套件。
// 没有分配时 Get 返回空字符串，Delete 幂等。
type TierStore interface {
	Get(ctx context.Context, businessLine, endUser string) (string, error)
	Put(ctx context.Context, businessLine, endUser, tier string, at time.Time) error
	Delete(ctx context.Context, businessLine, endUser string) error
}

type tierKey struct{ businessLine, endUser string }

type MemTiers struct {
	mu    sync.Mutex
	tiers map[tierKey]string
}

func NewMemTiers() *MemTiers { return &MemTiers{tiers: map[tierKey]string{}} }

func (s *MemTiers) Get(_ context.Context, bl, eu string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tiers[tierKey{bl, eu}], nil
}

func (s *MemTiers) Put(_ context.Context, bl, eu, tier string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tiers[tierKey{bl, eu}] = tier
	return nil
}

func (s *MemTiers) Delete(_ context.Context, bl, eu string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tiers, tierKey{bl, eu})
	return nil
}

// TierCacheTTL 与 serve 的配额用量缓存同为 5 秒；跨进程分配变更遵循配额的软上限。
const TierCacheTTL = 5 * time.Second

// TierCache 只缓存档位读，删除屏障始终直接读 lifecycle 存储。
// 全局世代避免为注销过的 EndUser 永久保留世代记录；其他键的写入只使在途读多查一次。
type TierCache struct {
	Store TierStore
	Clock clock.Clock

	mu         sync.Mutex
	generation uint64
	entries    map[tierKey]tierEntry
}

type tierEntry struct {
	tier string
	at   time.Time
}

func NewTierCache(s TierStore, c clock.Clock) *TierCache {
	return &TierCache{Store: s, Clock: c, entries: map[tierKey]tierEntry{}}
}

func (c *TierCache) Get(ctx context.Context, bl, eu string) (string, error) {
	k := tierKey{bl, eu}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		now := c.Clock.Now()
		c.mu.Lock()
		if e, ok := c.entries[k]; ok && now.Sub(e.at) < TierCacheTTL {
			c.mu.Unlock()
			return e.tier, nil
		}
		generation := c.generation
		c.mu.Unlock()
		tier, err := c.Store.Get(ctx, bl, eu)
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		if generation != c.generation {
			c.mu.Unlock()
			continue // 失效前发起的读既不回填，也不把旧档位交给调用方。
		}
		for key, e := range c.entries {
			if now.Sub(e.at) >= TierCacheTTL {
				delete(c.entries, key)
			}
		}
		c.entries[k] = tierEntry{tier: tier, at: now}
		c.mu.Unlock()
		return tier, nil
	}
}

// Invalidate 也供 Janitor 在存储事务结束后调用，防止提交前的读回填旧值。
func (c *TierCache) Invalidate(bl, eu string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	delete(c.entries, tierKey{bl, eu})
}

func (c *TierCache) Put(ctx context.Context, bl, eu, tier string, at time.Time) error {
	defer c.Invalidate(bl, eu) // 报错也可能已提交。
	return c.Store.Put(ctx, bl, eu, tier, at)
}

func (c *TierCache) Delete(ctx context.Context, bl, eu string) error {
	defer c.Invalidate(bl, eu)
	return c.Store.Delete(ctx, bl, eu)
}

// EffectiveTier 返回有效档位与日限额（元）；配置移除后回落默认，不改变历史用量。
func (q *Quotas) EffectiveTier(ctx context.Context, bl, eu string) (string, float64, error) {
	if q == nil {
		return "", 0, nil
	}
	l := q.Limits[bl]
	if eu != "" && q.Tiers != nil {
		tier, err := q.Tiers.Get(ctx, bl, eu)
		if err != nil {
			return "", 0, err
		}
		if tier != "" {
			if limits, ok := l.Tiers[tier]; ok {
				return tier, limits.EndUserDaily, nil
			}
			metrics.QuotaUnknownTier.WithLabelValues().Inc()
		}
	}
	return "", l.EndUserDaily, nil
}
