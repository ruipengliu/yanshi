package feed

import (
	"context"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
)

// Hub 是本进程各 Session 的共享事件流（延展性评审 §8.2）：同一 Session 的本地订阅者共用一个读者，它等待日志
// 前进、读一次新事件，再分发给每个订阅者。订阅数增加不增加日志的等待与读取；读者随第一个订阅者开始、随最后
// 一个离开而结束。
type Hub struct {
	Log eventlog.Log

	mu       sync.Mutex
	sessions map[string]*shared
}

type shared struct {
	// pos 是读者已读到并分发的位置。
	pos  uint64
	subs map[*follower]struct{}
	stop context.CancelFunc
}

// follower 是一个订阅者在共享流上的位置：读者把新事件成批送到 batches；缓冲已满时丢弃这一批、通知 lagged，
// 订阅者自己从日志补读（慢订阅者不拖慢其他订阅者，也不丢事件）。
type follower struct {
	batches chan []*v1.Event
	lagged  chan struct{}
}

const followerBuffer = 64

// join 加入 Session 的共享流，此后读到的新事件都送到返回的 follower。没有读者时以 after 为起点创建一个。
func (h *Hub) join(id string, after uint64) (*follower, func()) {
	f := &follower{batches: make(chan []*v1.Event, followerBuffer), lagged: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]*shared{}
	}
	sh := h.sessions[id]
	if sh == nil {
		ctx, cancel := context.WithCancel(context.Background())
		sh = &shared{pos: after, subs: map[*follower]struct{}{}, stop: cancel}
		h.sessions[id] = sh
		go h.read(ctx, id, sh)
	}
	sh.subs[f] = struct{}{}
	return f, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(sh.subs, f)
		if len(sh.subs) == 0 && h.sessions[id] == sh {
			delete(h.sessions, id)
			sh.stop()
		}
	}
}

// read 是 Session 的读者。等待或读取出错（如数据库短暂不可用）时退避后从同一位置重试。
func (h *Hub) read(ctx context.Context, id string, sh *shared) {
	backoff := waitRetryMin
	for {
		h.mu.Lock()
		pos := sh.pos
		h.mu.Unlock()
		_, err := h.Log.Wait(ctx, id, pos)
		var events []*v1.Event
		if err == nil {
			events, err = eventlog.ReadAll(ctx, h.Log, id, pos)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, waitRetryMax)
			continue
		}
		backoff = waitRetryMin
		if len(events) == 0 {
			continue
		}
		h.mu.Lock()
		sh.pos = events[len(events)-1].GetSeq()
		for f := range sh.subs {
			select {
			case f.batches <- events:
			default:
				select {
				case f.lagged <- struct{}{}:
				default:
				}
			}
		}
		h.mu.Unlock()
	}
}

// readers 返回正在运行的读者数（测试用）。
func (h *Hub) readers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}
