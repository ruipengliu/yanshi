// Package live 是 Session 级的实时增量扇出（模型 token 流等）。
// 增量是易失的：不进入 Event 日志，丢失后由随后提交的完整事件覆盖。
package live

import "sync"

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
