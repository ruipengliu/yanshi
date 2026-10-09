package sim

import (
	"context"
	"errors"
	"fmt"

	"yanshi/internal/eventlog"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/sdk/nodesdk"
)

// Session 生命周期的模拟（docs/design/m2-session-lifecycle.md §7）：随机关闭与删除 Session——
// 可能发生在 Run 进行中、设备执行中、沙箱执行中——并在 Janitor 的任意清理阶段注入崩溃。
// 被关闭或删除的 Session 由同一 EndUser 的新 Session 替换，使其余路径的覆盖不受影响。

func (w *World) newJanitor() *janitor.Janitor {
	w.nextJ++
	return &janitor.Janitor{
		ID: fmt.Sprintf("j%d", w.nextJ), Queue: w.janitorQueue, Service: w.svc, Store: w.store, Sessions: w.queue,
		SandboxQueue: w.sandboxQueue, Inbox: w.hub.Inbox, Nodes: w.hub.Dir, Sandbox: w.provider, Activity: w.activity,
		Ledger: w.ledger, Artifacts: w.artifacts, Index: w.index, Deletions: w.deletions, Clock: w.clock, LeaseTTL: leaseTTL,
		BeforeStage: func(context.Context, string) {
			if w.faults && w.crash != nil && w.chance(0.1) {
				w.crash()
			}
		},
	}
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
	id, err := w.svc.Create(context.Background(), service.CreateRequest{BusinessLine: "bl", EndUser: fmt.Sprintf("u%d", i), Agent: "sim"})
	if err != nil {
		return err
	}
	w.sessions[i] = id
	return nil
}

func (w *World) closeSession() error {
	i := w.rng.IntN(len(w.sessions))
	sid := w.sessions[i]
	if err := w.svc.Close(context.Background(), sid, "sim", "test"); err != nil {
		return fmt.Errorf("close %s: %w", sid, err)
	}
	w.Stats.Closes++
	w.closed = append(w.closed, sid)
	w.tracef("close %s", sid)
	return w.replace(i)
}

func (w *World) deleteSession() error {
	i := w.rng.IntN(len(w.sessions))
	sid := w.sessions[i]
	if err := w.svc.Delete(context.Background(), sid, "end_user", ""); err != nil {
		return fmt.Errorf("delete %s: %w", sid, err)
	}
	w.Stats.Deletions++
	w.deleted = append(w.deleted, sid)
	w.tracef("delete %s", sid)
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
