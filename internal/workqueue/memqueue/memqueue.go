// Package memqueue 是 workqueue.Queue 的进程内实现。按入队顺序认领，结果确定。
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
}

func New(c clock.Clock) *Queue { return &Queue{clock: c} }

var _ workqueue.Queue = (*Queue)(nil)

func (q *Queue) find(id string) (int, *item) {
	for i, it := range q.items {
		if it.sessionID == id {
			return i, it
		}
	}
	return -1, nil
}

func (q *Queue) Enqueue(_ context.Context, sessionID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, it := q.find(sessionID); it != nil {
		if it.leased {
			it.dirty = true
		}
		it.parkedUntil = time.Time{}
		return nil
	}
	q.items = append(q.items, &item{sessionID: sessionID})
	return nil
}

func (q *Queue) Claim(_ context.Context, holder string, ttl time.Duration) (*workqueue.Lease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock.Now()
	for _, it := range q.items {
		if it.leased && now.Before(it.expires) {
			continue
		}
		if !it.leased && now.Before(it.parkedUntil) {
			continue
		}
		q.next++
		it.leased, it.holder, it.token, it.expires, it.dirty = true, holder, q.next, now.Add(ttl), false
		return &workqueue.Lease{SessionID: it.sessionID, Holder: holder, Token: it.token, Expires: it.expires}, nil
	}
	return nil, workqueue.ErrEmpty
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
	it.leased = false
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
	it.leased, it.dirty, it.parkedUntil = false, false, time.Time{}
	// 移到队尾，避免同一 Session 饿死其他 Session。
	q.items = append(append(q.items[:i], q.items[i+1:]...), it)
	return nil
}
