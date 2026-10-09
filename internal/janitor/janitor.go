// Package janitor 在后台回收已关闭与已删除 Session 的资源
// （docs/design/m2-session-lifecycle.md §3.3、§5，ADR-0015）。
//
// Janitor 以工作队列认领 Session（键为 Session ID），与 Worker 一样以 Step 推进、可被任意进程接管。
// 每个清理阶段都是幂等的；删除流程的最后再扫一遍，清除"先写、后查"窗口中漏进来的写入。
package janitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/node"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/sdk/nodesdk"
)

type Janitor struct {
	ID    string
	Queue workqueue.Queue // 清理队列（pgqueue.Janitor）
	// Service 用于保留策略触发的关闭与删除。
	Service *service.Service
	Store   *session.Store
	// Sessions 是 Worker 的工作队列，SandboxQueue 是沙箱控制器的队列。
	Sessions     workqueue.Queue
	SandboxQueue workqueue.Queue
	Inbox        node.Inbox
	Nodes        node.Directory
	// Sandbox 为 nil 表示未启用沙箱。
	Sandbox   sandbox.Provider
	Activity  sandbox.Activity
	Ledger    nodesdk.Ledger // 沙箱控制器共享的去重账本
	Artifacts *artifact.Service
	// Memory 为 nil 表示未启用 Memory；删除 Session 时一并删除由它写入的 Memory（ADR-0016）。
	Memory    memory.Store
	Index     lifecycle.Index
	Deletions lifecycle.Deletions
	// Retention 是各业务线的保留策略，键为业务线名。
	Retention map[string]lifecycle.Retention
	Clock     clock.Clock
	Logger    *slog.Logger

	LeaseTTL time.Duration
	IdleWait time.Duration
	// SweepEvery 是保留策略与兜底扫描的间隔。
	SweepEvery time.Duration
	// BeforeStage 是测试钩子：每个清理阶段开始前调用，模拟测试在此注入崩溃。
	BeforeStage func(ctx context.Context, stage string)

	lease *workqueue.Lease
}

func (j *Janitor) init() {
	if j.LeaseTTL == 0 {
		j.LeaseTTL = 30 * time.Second
	}
	if j.IdleWait == 0 {
		j.IdleWait = time.Second
	}
	if j.SweepEvery == 0 {
		j.SweepEvery = time.Minute
	}
	if j.Logger == nil {
		j.Logger = slog.New(slog.DiscardHandler)
	}
}

// Run 持续执行 Step，并周期性执行 Sweep，直到 ctx 结束。
func (j *Janitor) Run(ctx context.Context) {
	j.init()
	var lastSweep time.Time
	for ctx.Err() == nil {
		did, err := j.Step(ctx)
		if err != nil && ctx.Err() == nil {
			j.Logger.Warn("janitor step failed", "janitor", j.ID, "err", err)
		}
		if now := j.Clock.Now(); now.Sub(lastSweep) >= j.SweepEvery {
			lastSweep = now
			if err := j.Sweep(ctx); err != nil && ctx.Err() == nil {
				j.Logger.Warn("janitor sweep failed", "err", err)
			}
		}
		if !did || err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(j.IdleWait):
			}
		}
	}
}

// Step 认领并处理一个 Session；返回 false 表示无事可做。处理失败时保留租约，下一步重试。
func (j *Janitor) Step(ctx context.Context) (bool, error) {
	j.init()
	if j.lease == nil {
		l, err := j.Queue.Claim(ctx, j.ID, j.LeaseTTL)
		if errors.Is(err, workqueue.ErrEmpty) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		j.lease = l
	} else if err := j.Queue.Renew(ctx, j.lease, j.LeaseTTL); err != nil {
		j.lease = nil
		if errors.Is(err, workqueue.ErrLeaseLost) {
			return true, nil
		}
		return false, err
	}
	if err := j.process(ctx, j.lease.SessionID); err != nil {
		return true, err
	}
	err := j.Queue.Release(ctx, j.lease, true)
	j.lease = nil
	if errors.Is(err, workqueue.ErrLeaseLost) {
		err = nil
	}
	return true, err
}

func (j *Janitor) stage(ctx context.Context, name string) error {
	if j.BeforeStage != nil {
		j.BeforeStage(ctx, name)
	}
	return ctx.Err()
}

func (j *Janitor) process(ctx context.Context, sid string) error {
	deleted, err := lifecycle.Deleted(ctx, j.Deletions, sid)
	if err != nil {
		return err
	}
	st, err := j.Store.Load(ctx, sid)
	if err != nil {
		return err
	}
	switch {
	case deleted:
		return j.delete(ctx, sid, st)
	case st.Closed != nil:
		// 关闭：销毁沙箱工作区，其余数据保留至删除。
		if err := j.stage(ctx, "close"); err != nil {
			return err
		}
		return j.destroySandbox(ctx, sid)
	}
	return nil
}

func (j *Janitor) delete(ctx context.Context, sid string, st *session.State) error {
	// 1. 中断活跃的 Run：持有它的 Worker 因 fencing 失效而停止，不再派发新调用。
	if err := j.stage(ctx, "interrupt"); err != nil {
		return err
	}
	if err := j.interrupt(ctx, st); err != nil {
		return err
	}
	// 2. 请求曾执行过调用的设备清除本地记录（设备离线时上线后送达）。
	if err := j.stage(ctx, "forget"); err != nil {
		return err
	}
	if err := j.forgetOnDevices(ctx, sid, st); err != nil {
		return err
	}
	// 3～5. 撤回待投递调用、移出队列、销毁沙箱、删除工件。
	if err := j.stage(ctx, "withdraw"); err != nil {
		return err
	}
	if err := j.sweep(ctx, sid); err != nil {
		return err
	}
	// 6. 删除日志与索引。
	if err := j.stage(ctx, "log"); err != nil {
		return err
	}
	if err := j.Store.Log.Delete(ctx, sid); err != nil {
		return err
	}
	if j.Store.Snapshots != nil {
		if err := j.Store.Snapshots.Delete(ctx, sid); err != nil {
			return err
		}
	}
	if err := j.Index.Delete(ctx, sid); err != nil {
		return err
	}
	// 7. 再扫一遍：清除"先写、后查"窗口中漏进来的写入。
	if err := j.stage(ctx, "sweep"); err != nil {
		return err
	}
	if err := j.sweep(ctx, sid); err != nil {
		return err
	}
	if err := j.Deletions.Complete(ctx, sid, j.Clock.Now()); err != nil {
		return err
	}
	j.Logger.Info("session deleted", "session", sid)
	return nil
}

func (j *Janitor) interrupt(ctx context.Context, st *session.State) error {
	for range 16 {
		a := st.Active()
		if st.Created == nil || a == nil {
			return nil
		}
		err := j.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_RunInterrupted{RunInterrupted: &v1.RunInterrupted{RunId: a.ID, By: "deletion"}}})
		if !errors.Is(err, eventlog.ErrConflict) {
			return err
		}
		if err := j.Store.SyncAfterConflict(ctx, st); err != nil {
			if errors.Is(err, session.ErrGone) {
				return nil
			}
			return err
		}
	}
	return errors.New("interrupt before deletion: too many conflicts")
}

func (j *Janitor) forgetOnDevices(ctx context.Context, sid string, st *session.State) error {
	seen := map[string]bool{}
	for _, r := range st.Runs {
		for _, c := range r.Calls {
			if c.NodeID == "" || sandbox.IsNode(c.NodeID) || seen[c.NodeID] {
				continue
			}
			seen[c.NodeID] = true
			if _, err := j.Nodes.Get(ctx, c.NodeID); errors.Is(err, node.ErrNotFound) {
				continue // Node 已随 EndUser 注销而删除
			} else if err != nil {
				return err
			}
			if err := j.Inbox.Put(ctx, c.NodeID, ForgetInvoke(sid)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ForgetInvoke 是请求设备清除某 Session 本地记录的平台调用。它本身不属于任何 Session，
// 因此不会随 Session 的清理被撤回。
func ForgetInvoke(sessionID string) *v1.Invoke {
	return &v1.Invoke{CallId: "forget_" + sessionID, Capability: nodesdk.ForgetCapability,
		ArgumentsJson: fmt.Sprintf(`{"session_id":%q}`, sessionID)}
}

// sweep 清除除日志与索引之外的全部数据；幂等，可重复执行。
func (j *Janitor) sweep(ctx context.Context, sid string) error {
	if err := j.Inbox.RemoveSession(ctx, sid); err != nil {
		return err
	}
	if err := j.Sessions.Remove(ctx, sid); err != nil {
		return err
	}
	if err := j.destroySandbox(ctx, sid); err != nil {
		return err
	}
	if err := j.Ledger.Forget(sid); err != nil {
		return err
	}
	// 快照含有对话内容：日志删除前后各删一次，第二遍清扫兜住删除期间写入的快照。
	if j.Store.Snapshots != nil {
		if err := j.Store.Snapshots.Delete(ctx, sid); err != nil {
			return err
		}
	}
	if j.Memory != nil {
		if err := j.Memory.DeleteSession(ctx, sid); err != nil {
			return err
		}
	}
	return j.Artifacts.DeleteSession(ctx, sid)
}

func (j *Janitor) destroySandbox(ctx context.Context, sid string) error {
	id := sandbox.NodeID(sid)
	if err := j.SandboxQueue.Remove(ctx, id); err != nil {
		return err
	}
	if j.Sandbox != nil {
		if err := j.Sandbox.Destroy(ctx, id); err != nil {
			return err
		}
	}
	return j.Activity.Delete(ctx, id)
}

// Sweep 执行保留策略（自动关闭与删除），重新入队未完成的删除（写入删除记录后、入队前崩溃的情况），
// 并按保留期清理沙箱账本。
func (j *Janitor) Sweep(ctx context.Context) error {
	j.init()
	now := j.Clock.Now()
	for bl, r := range j.Retention {
		if r.CloseAfterIdle > 0 {
			ids, err := j.Index.IdleBefore(ctx, bl, now.Add(-r.CloseAfterIdle), 100)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := j.Service.Close(ctx, id, "retention", "idle"); err != nil && !errors.Is(err, service.ErrNotFound) {
					return err
				}
			}
		}
		if r.DeleteAfterClose > 0 {
			ids, err := j.Index.ClosedBefore(ctx, bl, now.Add(-r.DeleteAfterClose), 100)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := j.Service.Delete(ctx, id, "retention", ""); err != nil && !errors.Is(err, service.ErrNotFound) {
					return err
				}
			}
		}
	}
	pending, err := j.Deletions.Pending(ctx, 100)
	if err != nil {
		return err
	}
	for _, id := range pending {
		if err := j.Queue.Enqueue(ctx, id); err != nil {
			return err
		}
	}
	return j.Ledger.Prune(now.Add(-nodesdk.DefaultLedgerRetention))
}
