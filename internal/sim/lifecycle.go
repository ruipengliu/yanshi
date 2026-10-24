package sim

import (
	"context"
	"errors"
	"fmt"
	"time"

	"yanshi/internal/eventlog"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/sdk/nodesdk"
)

// Session 生命周期的模拟（docs/design/m2-session-lifecycle.md §7）：随机关闭与删除 Session——
// 可能发生在 Run 进行中、设备执行中、沙箱执行中——并在 Janitor 的任意清理阶段注入崩溃。
// 被关闭或删除的 Session 由同一 EndUser 的新 Session 替换，使其余路径的覆盖不受影响。

func (w *World) newJanitor() *janitor.Janitor {
	w.nextJ++
	j := w.core.Janitor(fmt.Sprintf("j%d", w.nextJ), w.provider, nil, 0)
	j.BeforeStage = func(context.Context, string) {
		if w.faults && w.crash != nil && w.chance(0.1) {
			w.crash()
		}
	}
	return j
}

func (w *World) stepJanitor(i int) error {
	_, err := w.stepJanitorDid(i)
	return err
}

// stepJanitorDid 推进一个 Janitor，并报告它是否做了事（清理队列为空时为 false）。
func (w *World) stepJanitorDid(i int) (bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	crashed := false
	w.crash = func() { crashed = true; cancel() }
	defer func() { w.crash = nil; cancel() }()
	j := w.janitors[i]
	did, err := j.Step(ctx)
	if crashed {
		w.tracef("crash %s (mid-cleanup)", j.ID)
		w.Stats.JanitorCrashes++
		w.janitors[i] = w.newJanitor()
		return true, nil
	}
	if did {
		w.tracef("step %s", j.ID)
	}
	if err != nil {
		return did, fmt.Errorf("janitor %s step: %w", j.ID, err)
	}
	return did, nil
}

// replace 用同一 EndUser 的新 Session 替换 sessions[i]。
func (w *World) replace(i int) error {
	id, err := w.svc.Create(context.Background(), service.CreateRequest{BusinessLine: "bl", EndUser: w.users[i], Agent: "sim"})
	if err != nil {
		return err
	}
	w.sessions[i] = id
	return nil
}

func (w *World) closeSession() error {
	i := w.rng.IntN(len(w.sessions))
	return w.closeAt(i, w.svc.Close(context.Background(), w.sessions[i], "sim", "test"))
}

// closeAt 处理关闭第 i 个 Session 的结果 err：结果未知时客户端重试。
func (w *World) closeAt(i int, err error) error {
	sid := w.sessions[i]
	var cf cancelFault
	var sf *storeFault
	for range 6 {
		if !errors.As(err, &cf) && !errors.As(err, &sf) {
			break
		}
		// 关闭已写入、撤回委托失败，或写入的结果未知（存储故障）：客户端重试，关闭之后的步骤（撤回、索引、销毁工作区）
		// 须在重试中补上。
		w.tracef("close %s: %v; retrying", sid, err)
		err = w.svc.Close(context.Background(), sid, "sim", "test")
	}
	gaveUp := errors.As(err, &cf)
	if gaveUp {
		w.tracef("close %s: %v; giving up", sid, err)
		err = nil
	}
	if err != nil {
		return fmt.Errorf("close %s: %w", sid, err)
	}
	if !gaveUp {
		// 关闭之后的步骤都已完成：未了结的后台调用的投递已撤回，设备不得再执行它们（ADR-0035）。
		st, err := w.svc.Load(context.Background(), sid)
		if err != nil {
			return err
		}
		for id := range st.Background {
			w.withdrawn[id] = true
		}
	}
	w.Stats.Closes++
	w.closed = append(w.closed, sid)
	w.tracef("close %s", sid)
	return w.replace(i)
}

func (w *World) deleteSession() error {
	i := w.rng.IntN(len(w.sessions))
	sid := w.sessions[i]
	// 一部分删除是保留期到期（Janitor 的 Sweep 以 retention 为原因删除）：Session 写入的 Memory 保留（ADR-0037）。
	// 写过 Memory 的 Session 很少（多数 memory_save 被闸门要求审批或参数不合法），所以先以它的名义写入一条：每次到期删除
	// 都检验 Memory 是否保留。
	reason := "end_user"
	if w.chance(0.3) {
		reason = lifecycle.ReasonRetention
		w.nextInput++
		if _, err := w.memories.Save(context.Background(), memory.SaveRequest{BusinessLine: "bl", EndUser: w.users[i], SessionID: sid,
			CallID: fmt.Sprintf("retention-%d", w.nextInput), Category: "preference", Content: fmt.Sprintf("偏好 到期 %d", w.nextInput)}); err != nil {
			w.tracef("memory before retention %s: %v", sid, err) // 条数达到上限等：照常删除
		}
	}
	if err := w.svc.Delete(context.Background(), sid, reason, ""); err != nil {
		return fmt.Errorf("delete %s: %w", sid, err)
	}
	w.Stats.Deletions++
	w.deleted = append(w.deleted, sid)
	w.tracef("delete %s (%s)", sid, reason)
	return w.replace(i)
}

// lifecycleDone 报告清理队列已空、全部删除都已完成、设备 Inbox 中不再有待投递项（含清除本地记录的请求）。
func (w *World) lifecycleDone() (bool, error) {
	ctx := context.Background()
	if pending, err := w.deletions.Pending(ctx, 1); err != nil || len(pending) > 0 {
		return false, err
	}
	for _, n := range w.nodes {
		if items, _, err := w.hub.Inbox.Pending(ctx, n.id); err != nil || len(items) > 0 {
			return false, err
		}
	}
	return true, nil
}

// checkDeleted 检查已完成删除的 Session 在所有存储中都不再有数据，并且没有"复活"。
func (w *World) checkDeleted(final bool) error {
	ctx := context.Background()
	for _, sid := range w.deleted {
		t, err := w.deletions.Get(ctx, sid)
		if err != nil {
			return fmt.Errorf("invariant: deleted session %s has no tombstone: %w", sid, err)
		}
		if t.CompletedAt.IsZero() {
			continue
		}
		if events, _ := eventlog.ReadAll(ctx, w.log, sid, 0); len(events) > 0 {
			return fmt.Errorf("invariant: deleted session %s has %d events", sid, len(events))
		}
		if _, err := w.index.Get(ctx, sid); !errors.Is(err, lifecycle.ErrNotFound) {
			return fmt.Errorf("invariant: deleted session %s is still indexed", sid)
		}
		if list, _ := w.artifacts.List(ctx, sid); len(list) > 0 {
			return fmt.Errorf("invariant: deleted session %s has %d artifacts", sid, len(list))
		}
		if w.provider.Exists(sandbox.NodeID(sid)) {
			return fmt.Errorf("invariant: deleted session %s still has a sandbox workspace", sid)
		}
		boxes := []string{sandbox.NodeID(sid)}
		for _, n := range w.nodes {
			boxes = append(boxes, n.id)
		}
		for _, b := range boxes {
			items, _, _ := w.hub.Inbox.Pending(ctx, b)
			for _, it := range items {
				if it.GetSessionId() == sid {
					return fmt.Errorf("invariant: deleted session %s has call %s pending on %s", sid, it.GetCallId(), b)
				}
			}
		}
		if s, ok := w.store.Snapshots.(*session.MemSnapshots); ok && s.Has(sid) {
			return fmt.Errorf("invariant: deleted session %s still has a projection snapshot", sid)
		}
		// 按保留期到期删除时 Memory 保留，其余删除一并清除（ADR-0037）。账号注销另行删除该用户的全部 Memory。
		if s, ok := w.memories.Store.(*memory.MemStore); ok && s.HasSession(sid) {
			if t.Reason != lifecycle.ReasonRetention {
				return fmt.Errorf("invariant: deleted session %s still has memories", sid)
			}
			if final {
				w.Stats.RetentionKeptMemories++
			}
		}
		// 零时刻作为"现在"：连已过期、尚未清理的记录也算在内。
		if entries, _, _ := w.presence.Store.List(ctx, sid, time.Time{}); len(entries) > 0 {
			return fmt.Errorf("invariant: deleted session %s still has %d presence entries", sid, len(entries))
		}
		if l, ok := w.ledger.(*nodesdk.MemLedger); ok && l.HasSession(sid) {
			return fmt.Errorf("invariant: deleted session %s still in the sandbox ledger", sid)
		}
		if final {
			// 收敛后设备都已上线并处理了清除请求。
			for _, n := range w.nodes {
				if n.ledger.HasSession(sid) {
					return fmt.Errorf("invariant: device %s still holds records of deleted session %s", n.id, sid)
				}
			}
		}
	}
	if final {
		for _, sid := range w.closed {
			if w.provider.Exists(sandbox.NodeID(sid)) {
				return fmt.Errorf("invariant: closed session %s still has a sandbox workspace", sid)
			}
		}
	}
	return nil
}
