package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"yanshi/internal/lifecycle"
	"yanshi/internal/pg"
	"yanshi/internal/shard"
	"yanshi/internal/workqueue"
)

// ShardOptions 的时限只约束一次认领尝试；离线续扫与总预算由 C5b 补充。
// DeterministicQueues 只用于单线程模拟，按全局入队/归还顺序选择各库候选。
type ShardOptions struct {
	ClaimTimeout        time.Duration
	DeterministicQueues bool
}

type storeRouter struct {
	scanMu       sync.Mutex
	activityNext int
	locator      shard.Locator
	ids          []int
	stores       map[int]Stores
}

// Sharded 组合完整的单库存储，不拆开单库内的事务对象图。
// ctx 控制合并通知的生命周期；调用方须在关闭底层存储之前取消它。
func Sharded(ctx context.Context, locator shard.Locator, parts map[int]Stores, opts ShardOptions) (Stores, error) {
	ids := locator.Shards()
	if len(ids) == 0 || len(ids) != len(parts) {
		return Stores{}, fmt.Errorf("app: shard stores do not match locator")
	}
	r := &storeRouter{locator: locator, ids: ids, stores: make(map[int]Stores, len(parts))}
	for _, id := range ids {
		s, ok := parts[id]
		if !ok || s.Log == nil || s.Queue == nil || s.Waking == nil || s.Outbox == nil || s.Deletions == nil {
			return Stores{}, fmt.Errorf("app: shard %d is incomplete", id)
		}
		for _, component := range []any{s.Dir, s.Inbox, s.SandboxQueue, s.Ledger, s.Activity, s.ArtifactMeta, s.Index, s.JanitorQueue, s.Memories, s.Grants, s.Snapshots, s.Usage, s.Tiers, s.Presence, s.Push} {
			if component == nil {
				return Stores{}, fmt.Errorf("app: shard %d has missing stores", id)
			}
		}
		if _, ok := s.Deletions.(lifecycle.Watcher); !ok {
			return Stores{}, fmt.Errorf("app: shard %d lacks deletion watcher", id)
		}
		if _, ok := s.Deletions.(lifecycle.RequestProgress); !ok {
			return Stores{}, fmt.Errorf("app: shard %d lacks deletion progress", id)
		}
		for _, q := range []workqueue.Queue{s.Queue, s.SandboxQueue, s.JanitorQueue} {
			if _, ok := q.(workqueue.Deferrer); !ok {
				return Stores{}, fmt.Errorf("app: shard %d lacks queue deferrer", id)
			}
			if _, ok := q.(workqueue.Stats); !ok {
				return Stores{}, fmt.Errorf("app: shard %d lacks queue stats", id)
			}
			if _, ok := q.(workqueue.Inspector); !ok {
				return Stores{}, fmt.Errorf("app: shard %d lacks queue inspector", id)
			}
			if opts.DeterministicQueues {
				if _, ok := q.(workqueue.Peeker); !ok {
					return Stores{}, fmt.Errorf("app: shard %d lacks simulation peek", id)
				}
			}
		}
		r.stores[id] = s
	}
	if opts.ClaimTimeout <= 0 {
		opts.ClaimTimeout = time.Second
	}
	q := newRoutedQueue(ctx, r, func(s Stores) workqueue.Queue { return s.Queue }, false, opts)
	return Stores{
		Log: routedLog{r}, Waking: routedWakingWithQueue{r: r, queue: q}, Queue: q,
		SandboxQueue: newRoutedQueue(ctx, r, func(s Stores) workqueue.Queue { return s.SandboxQueue }, true, opts),
		JanitorQueue: newRoutedQueue(ctx, r, func(s Stores) workqueue.Queue { return s.JanitorQueue }, false, opts),
		Outbox:       routedOutbox{r}, Dir: routedDir{r}, Inbox: routedInbox{r}, Ledger: routedLedger{r},
		Activity: routedActivity{r}, ArtifactMeta: routedArtifactMeta{r}, Index: routedIndex{r},
		Deletions: routedDeletions{r}, Memories: routedMemories{r}, Grants: routedGrants{r},
		Snapshots: routedSnapshots{r}, Usage: routedUsage{r}, Tiers: routedTiers{r},
		Presence: routedPresence{r}, Push: routedPush{r}, shards: r,
	}, nil
}

func (r *storeRouter) pick(ctx context.Context, kind shard.Kind, key string) (Stores, error) {
	id, err := r.locator.Locate(shard.Target{Kind: kind, Key: key})
	if err != nil {
		return Stores{}, err
	}
	return r.at(ctx, id)
}

func (r *storeRouter) at(ctx context.Context, id int) (Stores, error) {
	s, ok := r.stores[id]
	if !ok {
		return Stores{}, fmt.Errorf("app: shard %d is not assembled", id)
	}
	if err := pg.CheckPool(ctx, s.pgPool); err != nil {
		return Stores{}, err
	}
	return s, nil
}

// each 用于可重复的独立维护；一库失败仍尝试其他库，但不会报告完整成功。
func (r *storeRouter) each(ctx context.Context, fn func(Stores) error) error {
	var errs []error
	for _, id := range r.ids {
		s, err := r.at(ctx, id)
		if err == nil {
			err = fn(s)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("shard %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Shards 供删除不变量逐个检查实际存储，避免只检查路由命中的正确副本而漏掉错库残留。
func (s Stores) Shards() []Stores {
	if s.shards == nil {
		return []Stores{s}
	}
	out := make([]Stores, 0, len(s.shards.ids))
	for _, id := range s.shards.ids {
		out = append(out, s.shards.stores[id])
	}
	return out
}

func sortedLimit(ids []string, limit int) []string {
	sort.Strings(ids)
	if limit >= 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}
