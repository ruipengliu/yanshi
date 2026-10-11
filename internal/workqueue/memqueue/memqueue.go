// Package memqueue 是 workqueue.Queue 的进程内实现。按优先级与入队顺序认领，结果确定。
package memqueue

import (
	"context"
	"sync"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/workqueue"
)

type item struct {
	sessionID string
	class     workqueue.Class
	holder    string
	token     uint64
	expires   time.Time
	leased    bool
	// dirty 表示租出期间被再次 Enqueue。
	dirty bool
	// parkedUntil 非零时，未租出的 item 在此之前不可认领。
	parkedUntil time.Time
}

type Queue struct {
	clock clock.Clock
	mu    sync.Mutex
	items []*item
	next  uint64
	ready chan struct{}
}

func New(c clock.Clock) *Queue { return &Queue{clock: c, ready: make(chan struct{}, 1)} }

var (
	_ workqueue.Queue    = (*Queue)(nil)
	_ workqueue.Signaler = (*Queue)(nil)
)

func (q *Queue) Ready() <-chan struct{} { return q.ready }

func (q *Queue) find(id string) (int, *item) {
	for i, it := range q.items {
		if it.sessionID == id {
			return i, it
		}
	}
	return -1, nil
}

func (q *Queue) Enqueue(_ context.Context, sessionID string, class workqueue.Class) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer func() {
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}()
	if _, it := q.find(sessionID); it != nil {
		if it.leased {
			it.dirty = true
		}
		it.parkedUntil, it.class = time.Time{}, class
		return nil
	}
	q.items = append(q.items, &item{sessionID: sessionID, class: class})
	return nil
}

func (q *Queue) Claim(_ context.Context, holder string, ttl time.Duration, pool workqueue.Pool) (*workqueue.Lease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock.Now()
	// items 按入队顺序排列：只在优先级严格更高时替换，选中的就是最高优先级中最早入队的。
	var best *item
	for _, it := range q.items {
		if it.leased && now.Before(it.expires) {
			continue
		}
		if !it.leased && now.Before(it.parkedUntil) {
			continue
		}
		if pool.Admits(it.class) && (best == nil || it.class.Priority > best.class.Priority) {
			best = it
		}
	}
	if best == nil {
		return nil, workqueue.ErrEmpty
	}
	q.next++
	best.leased, best.holder, best.token, best.expires, best.dirty = true, holder, q.next, now.Add(ttl), false
	return &workqueue.Lease{SessionID: best.sessionID, Class: best.class, Holder: holder, Token: best.token, Expires: best.expires}, nil
}

// held 返回 lease 仍然有效时对应的 item。
func (q *Queue) held(l *workqueue.Lease) (int, *item) {
	i, it := q.find(l.SessionID)
	if it == nil || !it.leased || it.token != l.Token || !q.clock.Now().Before(it.expires) {
		return -1, nil
	}
	return i, it
}

func (q *Queue) Renew(_ context.Context, l *workqueue.Lease, ttl time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, it := q.held(l)
	if it == nil {
		return workqueue.ErrLeaseLost
	}
	it.expires = q.clock.Now().Add(ttl)
	l.Expires = it.expires
	return nil
}

func (q *Queue) Park(_ context.Context, l *workqueue.Lease, until time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i, it := q.held(l)
	if it == nil {
		return workqueue.ErrLeaseLost
	}
	it.leased, it.class = false, l.Class
	if it.dirty {
		it.dirty, it.parkedUntil = false, time.Time{}
	} else {
		it.parkedUntil = until
	}
	q.items = append(append(q.items[:i], q.items[i+1:]...), it)
	return nil
}

func (q *Queue) Release(_ context.Context, l *workqueue.Lease, done bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i, it := q.held(l)
	if it == nil {
		return workqueue.ErrLeaseLost
	}
	if done && !it.dirty {
		q.items = append(q.items[:i], q.items[i+1:]...)
		return nil
	}
	it.leased, it.dirty, it.parkedUntil, it.class = false, false, time.Time{}, l.Class
	// 移到队尾，避免同一 Session 饿死其他 Session。
	q.items = append(append(q.items[:i], q.items[i+1:]...), it)
	return nil
}

func (q *Queue) Remove(_ context.Context, sessionID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if i, _ := q.find(sessionID); i >= 0 {
		q.items = append(q.items[:i], q.items[i+1:]...)
	}
	return nil
}
