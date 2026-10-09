// Package live 是 Session 级的实时增量扇出（模型 token 流等）。
// 增量是易失的：不进入 Event 日志，丢失后由随后提交的完整事件覆盖。
package live

import (
	"sync"

	v1 "yanshi/gen/yanshi/v1"
)

type Delta struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Attempt   uint32 `json:"attempt"`
	Text      string `json:"text"`
}

type Bus interface {
	Publish(d Delta)
	// Subscribe 返回该 Session 的增量通道与取消函数。慢订阅者会丢增量而不是阻塞发布者。
	Subscribe(sessionID string) (<-chan Delta, func())
}

type MemBus struct {
	mu   sync.Mutex
	subs map[string]map[chan Delta]struct{}
}

func NewMemBus() *MemBus { return &MemBus{subs: map[string]map[chan Delta]struct{}{}} }

func (b *MemBus) Publish(d Delta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[d.SessionID] {
		select {
		case ch <- d:
		default:
		}
	}
}

func (b *MemBus) Subscribe(sessionID string) (<-chan Delta, func()) {
	ch := make(chan Delta, 256)
	b.mu.Lock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[chan Delta]struct{}{}
	}
	b.subs[sessionID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs[sessionID], ch)
	}
}

// Discard 丢弃所有增量。
type Discard struct{}

func (Discard) Publish(Delta) {}
func (Discard) Subscribe(string) (<-chan Delta, func()) {
	return make(chan Delta), func() {}
}

// Follower 是 Bus 的可选接口：由订阅方告知当前产生增量的执行进程（AttemptStarted.live_endpoint），
// Bus 不必自己再读一遍日志（docs/design/m2-scale-test.md §6）。endpoint 为空表示当前没有执行中的 Attempt。
type Follower interface {
	SubscribeFollowing(sessionID string) (deltas <-chan Delta, follow func(endpoint string), cancel func())
}

// Endpoint 从 current 出发，按 events 推进"当前执行进程"：开始 Attempt 时切换到它的进程，
// 挂起或终态时清空。
func Endpoint(current string, events []*v1.Event) string {
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *v1.Event_AttemptStarted:
			current = p.AttemptStarted.GetLiveEndpoint()
		case *v1.Event_RunSuspended, *v1.Event_RunCompleted, *v1.Event_RunFailed, *v1.Event_RunInterrupted, *v1.Event_SessionClosed:
			current = ""
		}
	}
	return current
}
