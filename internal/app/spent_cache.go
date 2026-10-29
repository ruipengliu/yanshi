package app

import (
	"context"
	"sync"
	"time"
)

type spentKey struct {
	Version, Digest, BusinessLine, EndUser string
	From, To                               time.Time
}

type spentTotal struct {
	Shards  []int
	AsOf    time.Time
	Expires time.Time
	Value   int64
}

type spentCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[spentKey]spentTotal
}

func newSpentCache(opts ShardOptions) *spentCache {
	ttl, now := opts.UsageCacheTTL, opts.Now
	if ttl == 0 {
		ttl = 15 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &spentCache{ttl: ttl, now: now, entries: map[spentKey]spentTotal{}}
}

func (r *storeRouter) spent(ctx context.Context, line, user string, from, to time.Time) (int64, error) {
	version, digest := r.locator.Identity()
	key := spentKey{version, digest, line, user, from.UTC(), to.UTC()}
	cache := r.usageCache
	now := cache.now()
	// 模拟保留每一步的精确金额；生产软配额只复用有限期内的完整和。
	if !r.deterministic && cache.ttl > 0 {
		cache.mu.Lock()
		entry, ok := cache.entries[key]
		cache.mu.Unlock()
		if ok && now.Before(entry.Expires) {
			return entry.Value, nil
		}
	}
	parts, err := collect(ctx, r, "Usage.Spent", true, func(ctx context.Context, s Stores) (int64, error) { return s.Usage.Spent(ctx, line, user, from, to) })
	if err != nil {
		return 0, err
	}
	var total int64
	for _, part := range parts {
		total += part
	}
	if !r.deterministic && cache.ttl > 0 {
		cache.mu.Lock()
		// 从扫描开始计时；慢扫描不能把旧数据的有效期延长。
		for k, entry := range cache.entries {
			if !now.Before(entry.Expires) {
				delete(cache.entries, k)
			}
		}
		if len(cache.entries) >= 1024 {
			clear(cache.entries)
		}
		cache.entries[key] = spentTotal{Shards: append([]int(nil), r.ids...), AsOf: now, Expires: now.Add(cache.ttl), Value: total}
		cache.mu.Unlock()
	}
	return total, nil
}
