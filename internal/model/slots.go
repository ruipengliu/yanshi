package model

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// ErrBusy 表示并发名额在允许的等待时间内不可用，单次调用入口可将它映射为 HTTP 429。
var ErrBusy = errors.New("model slots busy")

// Slots 按键限制逻辑调用的并发，持有期间包含重试退避；零值可用。
// 同一键的正限额在仍有持有者或等待者时须一致，避免调用者用不同配置绕过上限。
// 空闲键立即移除，不保留业务线或声明名。
type Slots struct {
	mu   sync.Mutex
	keys map[string]*slotQueue
}

type slotQueue struct {
	limit    int
	inflight int
	waiting  []chan struct{}
}

// SlotReq 是一层并发约束；不同层须使用不同的键，声明的各版本应共用键。
type SlotReq struct {
	Key   string
	Limit int
	Wait  time.Duration
}

// Acquire 同键先来先得；limit <= 0 不限，wait <= 0 只尝试立即获取。
// 等待超时返回 ErrBusy，ctx 结束返回 ctx.Err()；成功返回的 release 可并发、重复调用。
func (s *Slots) Acquire(ctx context.Context, key string, limit int, wait time.Duration) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return func() {}, nil
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if s.keys == nil {
		s.keys = make(map[string]*slotQueue)
	}
	q := s.keys[key]
	if q == nil {
		q = &slotQueue{limit: limit}
		s.keys[key] = q
	} else if q.limit != limit {
		s.mu.Unlock()
		return nil, errors.New("model slots limit changed while key is active")
	}
	release = sync.OnceFunc(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.releaseLocked(key, q)
	})
	if q.inflight < q.limit && len(q.waiting) == 0 {
		q.inflight++
		s.mu.Unlock()
		return release, nil
	}
	if wait <= 0 {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ch := make(chan struct{})
	q.waiting = append(q.waiting, ch)
	s.mu.Unlock()
	select {
	case <-ch:
	case <-wctx.Done():
	}
	// 父 ctx 的取消与等待超时可同时发生；优先保留调用方的取消原因。
	err = ctx.Err()
	if err == nil && wctx.Err() != nil {
		err = ErrBusy
	}
	if err != nil {
		s.mu.Lock()
		i := slices.Index(q.waiting, ch)
		if i >= 0 {
			q.waiting = slices.Delete(q.waiting, i, i+1)
		} else {
			// 名额已交付但调用已放弃：按持有者释放，继续唤醒下一位。
			s.releaseLocked(key, q)
		}
		s.mu.Unlock()
		return nil, err
	}
	return release, nil
}

func (s *Slots) releaseLocked(key string, q *slotQueue) {
	q.inflight--
	if len(q.waiting) > 0 {
		ch := q.waiting[0]
		q.waiting[0] = nil
		q.waiting = q.waiting[1:]
		q.inflight++
		close(ch)
	} else if q.inflight == 0 {
		delete(s.keys, key)
	}
}

// AcquireAll 按给定顺序逐层获取；调用方须统一使用合计、业务线、声明名的顺序。
// 每层 Wait 独立计时，总时限由 ctx 控制；失败时逆序释放已持有的全部名额。
func (s *Slots) AcquireAll(ctx context.Context, reqs []SlotReq) (func(), error) {
	var releases []func()
	release := sync.OnceFunc(func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	})
	// 重复键会让同一次逻辑调用等待自己持有的名额，提前拒绝以免造成自锁。
	seen := make(map[string]bool)
	for _, req := range reqs {
		if req.Limit > 0 {
			if seen[req.Key] {
				return nil, errors.New("model slots: duplicate key in request")
			}
			seen[req.Key] = true
		}
	}
	for _, req := range reqs {
		r, err := s.Acquire(ctx, req.Key, req.Limit, req.Wait)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, r)
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
