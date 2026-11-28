package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/outbox"
	"yanshi/internal/sandbox"
	"yanshi/internal/shard"
	"yanshi/internal/workqueue"
)

type routedQueue struct {
	r       *storeRouter
	get     func(Stores) workqueue.Queue
	sandbox bool
	timeout time.Duration
	mu      sync.Mutex
	next    int
	ready   chan struct{}
	// 仅模拟维护全局逻辑顺序；生产只用每库的持久队列顺序与公平轮转。
	ordered  bool
	sequence uint64
	order    map[string]uint64
}

func newRoutedQueue(ctx context.Context, r *storeRouter, get func(Stores) workqueue.Queue, sandbox bool, opts ShardOptions) *routedQueue {
	q := &routedQueue{r: r, get: get, sandbox: sandbox, timeout: opts.ClaimTimeout,
		ready: make(chan struct{}, 1), ordered: opts.DeterministicQueues, order: map[string]uint64{}}
	for _, id := range r.ids {
		if signaler, ok := get(r.stores[id]).(workqueue.Signaler); ok {
			ch := signaler.Ready()
			if ch == nil {
				continue
			}
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case _, ok := <-ch:
						if !ok {
							return
						}
						select {
						case q.ready <- struct{}{}:
						default:
						}
					}
				}
			}()
		}
	}
	return q
}

func (q *routedQueue) target(key string) (int, error) {
	if q.sandbox {
		if !sandbox.IsNode(key) {
			return 0, fmt.Errorf("app: sandbox queue requires a sandbox Node ID")
		}
		key = sandbox.SessionOf(key)
	}
	return q.r.locator.Locate(shard.Target{Kind: shard.SessionData, Key: key})
}

func (q *routedQueue) pick(ctx context.Context, key string) (workqueue.Queue, int, error) {
	id, err := q.target(key)
	if err != nil {
		return nil, 0, err
	}
	s, err := q.r.at(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	return q.get(s), id, nil
}

func (q *routedQueue) remember(key string, move bool) {
	if !q.ordered {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.order[key]; !ok || move {
		q.sequence++
		q.order[key] = q.sequence
	}
}

func (q *routedQueue) forget(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.order, key)
}

func (q *routedQueue) Enqueue(ctx context.Context, key string, class workqueue.Class) error {
	ctx, cancel := q.r.attempt(ctx)
	defer cancel()
	part, _, err := q.pick(ctx, key)
	if err != nil {
		return err
	}
	if err := part.Enqueue(ctx, key, class); err != nil {
		return err
	}
	q.remember(key, false)
	return nil
}

func (q *routedQueue) Remove(ctx context.Context, key string) error {
	ctx, cancel := q.r.attempt(ctx)
	defer cancel()
	part, _, err := q.pick(ctx, key)
	if err != nil {
		return err
	}
	if err := part.Remove(ctx, key); err != nil {
		return err
	}
	q.forget(key)
	return nil
}

// Claim 每次只占用一个分片的尝试时限，空库或坏库之后继续；不持锁等待数据库。
func (q *routedQueue) Claim(ctx context.Context, holder string, ttl time.Duration, pool workqueue.Pool) (*workqueue.Lease, error) {
	if q.ordered {
		return q.claimOrdered(ctx, holder, ttl, pool)
	}
	ctx, cancel := context.WithTimeout(ctx, q.r.fanout.Budget)
	defer cancel()
	q.mu.Lock()
	start := q.next
	q.next = (q.next + 1) % len(q.r.ids)
	q.mu.Unlock()
	var errs []error
	for offset := range q.r.ids {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(append(errs, shardFailure(q.r.ids[(start+offset)%len(q.r.ids)], err))...)
		}
		pos := (start + offset) % len(q.r.ids)
		id := q.r.ids[pos]
		attempt, cancel := context.WithTimeout(ctx, q.timeout)
		s, err := q.r.at(attempt, id)
		if err != nil {
			cancel()
			err = shardFailure(id, err)
			if !onlyUnavailable(err) {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		lease, err := q.get(s).Claim(attempt, holder, ttl, pool)
		if err != nil && !errors.Is(err, workqueue.ErrEmpty) {
			err = shardFailure(id, err)
		}
		cancel()
		if err == nil {
			owner, locateErr := q.target(lease.SessionID)
			if locateErr != nil || owner != id {
				return nil, fmt.Errorf("app: claimed work belongs to another shard")
			}
			lease.Shard, lease.Sharded = id, true
			q.mu.Lock()
			q.next = (pos + 1) % len(q.r.ids)
			q.mu.Unlock()
			return lease, nil
		}
		if !errors.Is(err, workqueue.ErrEmpty) {
			if !onlyUnavailable(err) {
				return nil, err
			}
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return nil, workqueue.ErrEmpty
}

func (q *routedQueue) origin(ctx context.Context, l *workqueue.Lease) (workqueue.Queue, error) {
	if l == nil || !l.Sharded {
		return nil, workqueue.ErrLeaseLost
	}
	id, err := q.target(l.SessionID)
	if err != nil {
		return nil, err
	}
	if id != l.Shard {
		return nil, workqueue.ErrLeaseLost
	}
	s, err := q.r.at(ctx, l.Shard)
	if err != nil {
		return nil, err
	}
	return q.get(s), nil
}

func (q *routedQueue) Renew(ctx context.Context, l *workqueue.Lease, ttl time.Duration) error {
	ctx, cancel := q.r.attempt(ctx)
	defer cancel()
	part, err := q.origin(ctx, l)
	if err != nil {
		return err
	}
	return part.Renew(ctx, l, ttl)
}

func (q *routedQueue) Park(ctx context.Context, l *workqueue.Lease, until time.Time) error {
	ctx, cancel := q.r.attempt(ctx)
	defer cancel()
	part, err := q.origin(ctx, l)
	if err != nil {
		return err
	}
	if err := part.Park(ctx, l, until); err != nil {
		return err
	}
	q.remember(l.SessionID, true)
	return nil
}

func (q *routedQueue) Release(ctx context.Context, l *workqueue.Lease, done bool) error {
	ctx, cancel := q.r.attempt(ctx)
	defer cancel()
	part, err := q.origin(ctx, l)
	if err != nil {
		return err
	}
	if err := part.Release(ctx, l, done); err != nil {
		return err
	}
	if q.ordered {
		has, err := part.(workqueue.Inspector).Contains(ctx, l.SessionID)
		if err != nil {
			return err
		}
		if has {
			q.remember(l.SessionID, true)
		} else {
			q.forget(l.SessionID)
		}
	}
	return nil
}

func (q *routedQueue) Ready() <-chan struct{} { return q.ready }
func (q *routedQueue) Horizon() time.Duration {
	return q.get(q.r.stores[q.r.ids[0]]).(workqueue.Deferrer).Horizon()
}
func (q *routedQueue) Contains(ctx context.Context, key string) (bool, error) {
	ctx, cancel := q.r.attempt(ctx)
	defer cancel()
	part, _, err := q.pick(ctx, key)
	if err != nil {
		return false, err
	}
	return part.(workqueue.Inspector).Contains(ctx, key)
}
func (q *routedQueue) Promote(ctx context.Context, before time.Time, limit int) (int, error) {
	parts, err := collect(ctx, q.r, "Queue.Promote", false, func(ctx context.Context, s Stores) (int, error) {
		return q.get(s).(workqueue.Deferrer).Promote(ctx, before, limit)
	})
	var count int
	for _, n := range parts {
		count += n
	}
	return count, err
}

func (q *routedQueue) Depths(ctx context.Context) ([]workqueue.Depth, error) {
	parts, err := collect(ctx, q.r, "Queue.Depths", true, func(ctx context.Context, s Stores) ([]workqueue.Depth, error) {
		return q.get(s).(workqueue.Stats).Depths(ctx)
	})
	if err != nil {
		return nil, err
	}
	byClass := map[workqueue.Class]workqueue.Depth{}
	for _, rows := range parts {
		for _, row := range rows {
			d := byClass[row.Class]
			d.Class, d.Ready, d.OldestWait = row.Class, d.Ready+row.Ready, max(d.OldestWait, row.OldestWait)
			byClass[row.Class] = d
		}
	}
	var rows []workqueue.Depth
	for _, d := range byClass {
		rows = append(rows, d)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Class.BusinessLine != rows[j].Class.BusinessLine {
			return rows[i].Class.BusinessLine < rows[j].Class.BusinessLine
		}
		return rows[i].Class.Priority > rows[j].Class.Priority
	})
	return rows, nil
}

func (q *routedQueue) claimOrdered(ctx context.Context, holder string, ttl time.Duration, pool workqueue.Pool) (*workqueue.Lease, error) {
	// Peek 不认领、不改变租约或入队序号；模拟在单线程中查询候选后精确认领选中的键。
	var best *workqueue.Lease
	var source int
	for _, id := range q.r.ids {
		s, err := q.r.at(ctx, id)
		if err != nil {
			return nil, err
		}
		candidate, err := q.get(s).(workqueue.Peeker).Peek(ctx, pool)
		if errors.Is(err, workqueue.ErrEmpty) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if best == nil || candidate.Class.Priority > best.Class.Priority ||
			candidate.Class.Priority == best.Class.Priority && q.order[candidate.SessionID] < q.order[best.SessionID] {
			best, source = candidate, id
		}
	}
	if best == nil {
		return nil, workqueue.ErrEmpty
	}
	pool.SessionID = best.SessionID
	lease, err := q.get(q.r.stores[source]).Claim(ctx, holder, ttl, pool)
	if err == nil {
		owner, locateErr := q.target(lease.SessionID)
		if locateErr != nil || owner != source {
			return nil, fmt.Errorf("app: claimed work belongs to another shard")
		}
		lease.Shard, lease.Sharded = source, true
	}
	return lease, err
}

type routedWakingWithQueue struct {
	r     *storeRouter
	queue *routedQueue
}

var _ eventlog.WakingLog = routedWakingWithQueue{}

func (x routedWakingWithQueue) AppendAndEnqueue(ctx context.Context, sessionID string, expectedSeq uint64, class workqueue.Class, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	ctx, cancel := x.r.attempt(ctx)
	defer cancel()
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return 0, err
	}
	head, err := s.Waking.AppendAndEnqueue(ctx, sessionID, expectedSeq, class, envelopes, events...)
	if err == nil {
		x.queue.remember(sessionID, false)
	}
	return head, err
}

func (x routedLog) HeadHint(sessionID string) (uint64, bool) {
	s, err := x.r.pick(context.Background(), shard.SessionData, sessionID)
	if err != nil {
		return 0, false
	}
	if h, ok := s.Log.(eventlog.HeadHinter); ok {
		return h.HeadHint(sessionID)
	}
	return 0, false
}
