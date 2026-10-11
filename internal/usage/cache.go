package usage

import (
	"context"
	"sync"
	"time"

	"yanshi/internal/clock"
)

// SpentCache 包装 Store，缓存 Spent 的结果 TTL（延展性评审 §4.3）：配额在每次提交与每次模型调用之前检查，
// 不缓存时每个 Run 要多花约 8 次求和查询。经它写入的用量同时计入缓存，本进程的花费因此立即可见；其他进程的花费
// 至多晚 TTL 可见，超出量相应增加，配额本就是软上限（ADR-0019）。匿名化等直接作用于 Store 的修改只会使缓存偏高，
// 偏于保守。Meter 与 Quotas 须共用同一个 SpentCache。
type SpentCache struct {
	Store
	TTL   time.Duration
	Clock clock.Clock

	mu      sync.Mutex
	entries map[spentKey]*spentEntry
}

type spentKey struct {
	businessLine, endUser string
	from, to              time.Time
}

type spentEntry struct {
	spent int64
	at    time.Time
}

func NewSpentCache(s Store, ttl time.Duration, c clock.Clock) *SpentCache {
	return &SpentCache{Store: s, TTL: ttl, Clock: c, entries: map[spentKey]*spentEntry{}}
}

func (c *SpentCache) Spent(ctx context.Context, businessLine, endUser string, from, to time.Time) (int64, error) {
	k := spentKey{businessLine, endUser, from, to}
	now := c.Clock.Now()
	c.mu.Lock()
	if e := c.entries[k]; e != nil && now.Sub(e.at) < c.TTL {
		c.mu.Unlock()
		return e.spent, nil
	}
	c.mu.Unlock()
	v, err := c.Store.Spent(ctx, businessLine, endUser, from, to)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 过期的条目同时清掉：周期（日、月）滚动后旧键不再被查询。
	for key, e := range c.entries {
		if now.Sub(e.at) >= c.TTL {
			delete(c.entries, key)
		}
	}
	c.entries[k] = &spentEntry{spent: v, at: now}
	return v, nil
}

func (c *SpentCache) Record(ctx context.Context, e *Entry) error {
	if err := c.Store.Record(ctx, e); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.entries {
		if k.businessLine == e.BusinessLine && (k.endUser == "" || k.endUser == e.EndUser) && !e.At.Before(k.from) && e.At.Before(k.to) {
			v.spent += e.Cost
		}
	}
	return nil
}
