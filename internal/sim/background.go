package sim

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
	"yanshi/sdk/nodesdk"
)

// 后台调用的模拟（ADR-0035）：设备随机长时间离线使调用转入后台，客户端随机取消，时钟跳跃触发到期，时钟有时恰好停在
// 宽限期的边界上，设备在边界上上线或下线；设备下线时唤醒等它的 Session。

const (
	// bgGrace 是宽限期（与默认值相同）；bgTimeout 是后台时限，取得比默认值短得多，使到期在模拟的时长内发生。
	bgGrace   = 20 * time.Second
	bgTimeout = 5 * time.Minute
)

// wakeCounter 是 Hub 在设备下线时唤醒 Session 所用的队列：统计唤醒次数（DisconnectWakes）。
type wakeCounter struct {
	workqueue.Queue
	w *World
}

func (q wakeCounter) Enqueue(ctx context.Context, sessionID string, class workqueue.Class) error {
	q.w.Stats.DisconnectWakes++
	q.w.tracef("disconnect wake %s", sessionID)
	return q.Queue.Enqueue(ctx, sessionID, class)
}

// cancelBackground 取消一个未了结的后台调用；有时再取消一次，须为 conflict。取消之后，设备不得再执行它。
func (w *World) cancelBackground() error {
	ctx := context.Background()
	for _, sid := range w.live() {
		st, err := w.svc.Load(ctx, sid)
		if err != nil {
			return err
		}
		open := st.OpenBackground()
		if len(open) == 0 {
			continue
		}
		b := open[w.rng.IntN(len(open))]
		err = w.svc.CancelBackground(ctx, sid, b.CallID, "sim")
		if errors.Is(err, service.ErrConflict) {
			return nil // 与设备结果、到期竞争：已了结
		}
		if err != nil {
			return fmt.Errorf("cancel background %s: %w", b.CallID, err)
		}
		w.Stats.BackgroundCancels++
		w.withdrawn[b.CallID] = true
		w.tracef("cancel background %s/%s", sid, b.CallID)
		if w.chance(0.3) {
			if err := w.svc.CancelBackground(ctx, sid, b.CallID, "sim"); !errors.Is(err, service.ErrConflict) {
				return fmt.Errorf("invariant: second cancel of background call %s = %v, want conflict", b.CallID, err)
			}
		}
		return nil
	}
	return nil
}

// graceEdge 把时钟拨到某个等离线设备的调用的宽限期边界附近（前一秒、恰好、后一秒），有时让设备恰在此时上线；
// 或让一台有调用在等它的在线设备下线（执行中途下线，宽限期从下线时起算）。
func (w *World) graceEdge() error {
	ctx := context.Background()
	for _, sid := range w.live() {
		st, err := w.svc.Load(ctx, sid)
		if err != nil {
			return err
		}
		a := st.Active()
		if a == nil {
			continue
		}
		c := a.PendingCall()
		if c == nil || !c.Dispatched() {
			continue
		}
		n := w.nodeByID(c.NodeID)
		if n == nil {
			continue
		}
		if n.conn != nil {
			if w.chance(0.5) {
				w.disconnect(n)
			}
			return nil
		}
		info, err := w.stores.Dir.Get(ctx, n.id)
		if err != nil {
			return err
		}
		since := c.DispatchedAt
		if info.LastSeen.After(since) {
			since = info.LastSeen
		}
		edge := since.Add(bgGrace).Add(time.Duration(w.rng.IntN(3)-1) * time.Second)
		if d := edge.Sub(w.clock.Now()); d > 0 {
			w.clock.Advance(d)
			w.tracef("clock +%s (grace edge of %s)", d, c.Call.GetCallId())
		}
		if w.chance(0.5) {
			return w.connect(n)
		}
		return nil
	}
	return nil
}

// checkNotWithdrawn 在设备执行调用时检查它没有被撤回：取消与关闭先撤回投递，之后设备收不到它；被放回 Inbox
// （如过时的派发与取消交错）就会在用户取消之后执行。
func (w *World) checkNotWithdrawn() {
	if w.withdrawn[w.currentCall] {
		w.violations = append(w.violations, fmt.Sprintf("withdrawn background call %s was executed", w.currentCall))
	}
}

func (w *World) nodeByID(id string) *simNode {
	for _, n := range w.nodes {
		if n.id == id {
			return n
		}
	}
	return nil
}

// checkBackground 检查后台调用的不变量（ADR-0035）：
//   - 转入后台的只有设备调用，委托派生的子 Session 中没有；
//   - 转入后台之后，该调用不再有任何 ToolResult；
//   - 每个后台调用至多了结一次（结果、到期、取消三者之一），了结的都是转入过后台的调用；
//   - 没有活跃 Run、但有未了结后台调用的 Session 仍在队列中（停放到截止时间），否则没有人在截止时间叫醒它。
//
// current 表示 sid 是当前的 Session（没有被删除、替换）：只对它检查队列。
func (w *World) checkBackground(sid string, events []*v1.Event, st *session.State, current bool) error {
	bg := map[string]bool{}
	settled := map[string]bool{}
	for _, e := range events {
		if r := e.GetToolResult(); r != nil {
			if bg[r.GetCallId()] {
				return fmt.Errorf("invariant: call %s in %s has a result after it went to the background (seq %d)", r.GetCallId(), sid, e.GetSeq())
			}
			if b := r.GetBackgrounded(); b != nil {
				if st.Created.GetParent() != nil {
					return fmt.Errorf("invariant: delegated session %s backgrounded call %s", sid, r.GetCallId())
				}
				bg[r.GetCallId()] = true
			}
		}
		id := ""
		if r := nodesdk.BackgroundResultOf(e); r != nil {
			id = r.GetCallId()
		}
		if c := e.GetBackgroundCancelled(); c != nil {
			id = c.GetCallId()
		}
		if id == "" {
			continue
		}
		if !bg[id] || settled[id] {
			return fmt.Errorf("invariant: background call %s in %s settled twice or without going to the background (seq %d)", id, sid, e.GetSeq())
		}
		settled[id] = true
	}
	if q, ok := w.queue.(*memqueue.Queue); ok && current && st.Closed == nil && st.Active() == nil && len(st.OpenBackground()) > 0 && !q.Has(sid) {
		return fmt.Errorf("invariant: %s has %d open background calls but left the work queue", sid, len(st.OpenBackground()))
	}
	return nil
}

// checkBackgroundSettled 是活性：故障停止、设备都已上线之后，每个后台调用都最终了结（设备结果或到期）。
func (w *World) checkBackgroundSettled() error {
	for _, sid := range w.live() {
		st, err := w.svc.Load(context.Background(), sid)
		if err != nil {
			return err
		}
		if open := st.OpenBackground(); len(open) > 0 {
			return fmt.Errorf("liveness: %s still has %d open background calls (first %s, deadline %s)", sid, len(open), open[0].CallID, open[0].Deadline)
		}
	}
	return nil
}

// countBackground 统计日志中的后台调用路径。
func (w *World) countBackground(events []*v1.Event) {
	for _, e := range events {
		if e.GetToolResult().GetBackgrounded() != nil {
			w.Stats.Backgrounded++
		}
		switch nodesdk.BackgroundResultOf(e).GetReason() {
		case session.BackgroundReasonResult:
			w.Stats.BackgroundResults++
		case session.BackgroundReasonTimeout:
			w.Stats.BackgroundTimeouts++
		}
	}
}

// fingerprintBackground 把未了结的后台调用与设备 Inbox（含截止时间）写入差分指纹：内存与 PostgreSQL 的 Inbox 须同样地
// 只延长截止时间。
func (w *World) fingerprintBackground(h hash.Hash, sid string) {
	if st, err := w.store.Load(context.Background(), sid); err == nil {
		for _, b := range st.OpenBackground() {
			fmt.Fprintf(h, "bg|%s|%s|%s|%d;", b.CallID, b.RunID, b.NodeID, b.Deadline.Unix())
		}
	}
}

func (w *World) fingerprintInboxes(h hash.Hash) {
	for _, n := range w.nodes {
		items, _, _ := w.hub.Inbox.Pending(context.Background(), n.id)
		for _, it := range items {
			fmt.Fprintf(h, "inbox|%s|%s|%d;", n.id, it.GetCallId(), it.GetDeadline().GetSeconds())
		}
	}
}
