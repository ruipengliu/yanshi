// Package live 是 Session 级的实时增量扇出（模型 token 流等）。
// 增量是易失的：不进入 Event 日志，丢失后由随后提交的完整事件覆盖。
package live

import (
	"strings"
	"sync"

	v1 "yanshi/gen/yanshi/v1"
)

type Delta struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Attempt   uint32 `json:"attempt"`
	// AfterSeq 是这次生成接在日志中的位置：同一 (RunID, Attempt, AfterSeq) 的增量属于同一条消息。
	AfterSeq uint64 `json:"after_seq,omitempty"`
	Text     string `json:"text,omitempty"`
	// Snapshot 为 true 时 Text 是这条消息到目前为止的全文，由 Subscribe 补发给晚加入的订阅者。
	Snapshot bool `json:"snapshot,omitempty"`
	// End 表示这次生成结束（提交或失败），之后不再有它的增量。
	End bool `json:"end,omitempty"`
}

// maxDraft 限制为晚加入者缓存的草稿长度；超出后不再补发全文（订阅者等提交的事件）。
const maxDraft = 256 << 10

// draft 是某个 Session 正在生成的消息到目前为止的文本。
type draft struct {
	run      string
	attempt  uint32
	after    uint64
	text     strings.Builder
	overflow bool
}

func (d *draft) same(x Delta) bool {
	return d.run == x.RunID && d.attempt == x.Attempt && d.after == x.AfterSeq
}

type Bus interface {
	Publish(d Delta)
	// Subscribe 返回该 Session 的增量通道与取消函数。慢订阅者会丢增量而不是阻塞发布者。
	Subscribe(sessionID string) (<-chan Delta, func())
}

// MemBus 是进程内的 Bus。它为每个 Session 缓存正在生成的消息，新订阅者先收到一条 Snapshot，
// 再接着收到之后的增量；缓存与扇出在同一把锁下，二者之间不会漏也不会重。
type MemBus struct {
	mu     sync.Mutex
	subs   map[string]map[chan Delta]struct{}
	drafts map[string]*draft
}

func NewMemBus() *MemBus {
	return &MemBus{subs: map[string]map[chan Delta]struct{}{}, drafts: map[string]*draft{}}
}

func (b *MemBus) Publish(d Delta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch cur := b.drafts[d.SessionID]; {
	case d.Snapshot:
		// 从其他进程转来的补发不进入本进程的缓存。
	case d.End:
		delete(b.drafts, d.SessionID)
	case cur == nil || !cur.same(d):
		cur = &draft{run: d.RunID, attempt: d.Attempt, after: d.AfterSeq}
		cur.text.WriteString(d.Text)
		b.drafts[d.SessionID] = cur
	case !cur.overflow:
		if cur.text.Len()+len(d.Text) > maxDraft {
			cur.overflow = true
		} else {
			cur.text.WriteString(d.Text)
		}
	}
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
	if d := b.drafts[sessionID]; d != nil && !d.overflow && d.text.Len() > 0 {
		ch <- Delta{SessionID: sessionID, RunID: d.run, Attempt: d.attempt, AfterSeq: d.after, Text: d.text.String(), Snapshot: true}
	}
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
