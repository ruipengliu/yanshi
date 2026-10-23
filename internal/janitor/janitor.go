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
	"slices"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/clock"
	"yanshi/internal/delegation"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/node"
	"yanshi/internal/presence"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
	"yanshi/sdk/nodesdk"
)

type Janitor struct {
	ID    string
	Queue workqueue.Queue // 清理队列（pgqueue.Janitor）
	// Service 用于保留策略触发的关闭与删除。
	Service *service.Service
	Store   *session.Store
	// Data 是除日志与索引之外存放 Session 数据的全部地方，删除 Session 时按顺序清除（app.SessionData）。
	Data []lifecycle.Holder
	// Workspaces 在 Session 关闭时销毁其沙箱工作区（删除时它也在 Data 中）。
	Workspaces sandbox.Workspaces
	// Inbox 与 Nodes 用于请求设备清除本地记录。
	Inbox node.Inbox
	Nodes node.Directory
	// Ledger 是沙箱控制器共享的去重账本，定期清理过期记录。
	Ledger nodesdk.Ledger
	// Queues 是统计积压的队列，键为指标中的队列名。
	Queues    map[string]workqueue.Queue
	Index     lifecycle.Index
	Deletions lifecycle.Deletions
	// Retention 是各业务线的保留策略，键为业务线名。
	Retention map[string]lifecycle.Retention
	// Usage 非 nil 时清理早于 UsageRetention 的用量（docs/design/m4-quota-usage.md §4）。
	Usage          usage.Store
	UsageRetention time.Duration
	// Presence 非 nil 时清理过期的在场记录（docs/design/m3-duplex-channel.md §6）。
	Presence presence.Store
	Clock    clock.Clock
	Logger   *slog.Logger

	LeaseTTL time.Duration
	IdleWait time.Duration
	// SweepEvery 是保留策略与兜底扫描的间隔。
	SweepEvery time.Duration
	// StatsEvery 是统计各队列积压（yanshi_queue_ready、yanshi_queue_oldest_wait_seconds）的间隔，默认 15 秒。
	// 统计可能扫描整个队列，因此只由 Janitor 所在的少数进程做。
	StatsEvery time.Duration
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
	if j.StatsEvery == 0 {
		j.StatsEvery = 15 * time.Second
	}
	if j.Logger == nil {
		j.Logger = slog.New(slog.DiscardHandler)
	}
}

// Run 持续执行 Step，并周期性执行 Sweep，直到 ctx 结束。
func (j *Janitor) Run(ctx context.Context) {
	j.init()
	defer j.release(ctx)
	var lastSweep, lastStats time.Time
	for ctx.Err() == nil {
		if now := j.Clock.Now(); now.Sub(lastStats) >= j.StatsEvery {
			lastStats = now
			j.sampleDepths(ctx)
		}
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

// sampleDepths 统计 Session、沙箱与清理队列的积压，写入指标。
func (j *Janitor) sampleDepths(ctx context.Context) {
	metrics.QueueReady.Reset()
	metrics.QueueOldestWait.Reset()
	for name, q := range j.Queues {
		st, ok := q.(workqueue.Stats)
		if !ok {
			continue
		}
		depths, err := st.Depths(ctx)
		if err != nil {
			j.Logger.Warn("queue depth sampling failed", "queue", name, "err", err)
			continue
		}
		for _, d := range depths {
			metrics.QueueReady.WithLabelValues(name, d.Class.BusinessLine, d.Class.Priority.String()).Set(float64(d.Ready))
			metrics.QueueOldestWait.WithLabelValues(name, d.Class.BusinessLine, d.Class.Priority.String()).Set(d.OldestWait.Seconds())
		}
	}
}

// release 在停机时归还持有的租约，使其他 Janitor 立即接手。清理是可重复的，接手者从头再做一遍。
func (j *Janitor) release(ctx context.Context) {
	if j.lease == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := j.Queue.Release(rctx, j.lease, false); err != nil && !errors.Is(err, workqueue.ErrLeaseLost) {
		j.Logger.Warn("janitor release failed", "janitor", j.ID, "err", err)
	}
	j.lease = nil
}

// Step 认领并处理一个 Session；返回 false 表示无事可做。处理失败时保留租约，下一步重试。
func (j *Janitor) Step(ctx context.Context) (bool, error) {
	j.init()
	if j.lease == nil {
		l, err := j.Queue.Claim(ctx, j.ID, j.LeaseTTL, workqueue.Pool{})
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
		t, err := j.Deletions.Get(ctx, sid)
		if err != nil {
			return err
		}
		return j.delete(ctx, sid, st, t.Reason)
	case st.Closed != nil:
		// 关闭：销毁沙箱工作区，其余数据保留至删除。
		if err := j.stage(ctx, "close"); err != nil {
			return err
		}
		return j.Workspaces.DeleteSession(ctx, sid)
	}
	return nil
}

// delete 清除 Session 的全部数据。reason 是删除记录的原因：按保留期到期删除时，登记为 KeepOnRetention 的数据保留
// （ADR-0037：到期清理不是用户的删除意图）。
func (j *Janitor) delete(ctx context.Context, sid string, st *session.State, reason string) error {
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
	// 2'. 委托派生的子 Session 一并删除（docs/design/m5-multi-agent.md §4）：它们各自由 Janitor 清理，子的子同样级联。
	if err := j.stage(ctx, "children"); err != nil {
		return err
	}
	if err := j.deleteChildren(ctx, sid, st, reason); err != nil {
		return err
	}
	// 3～5. 撤回待投递调用、移出队列、销毁沙箱、删除工件。
	if err := j.stage(ctx, "withdraw"); err != nil {
		return err
	}
	if err := j.sweep(ctx, sid, reason); err != nil {
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
	if err := j.sweep(ctx, sid, reason); err != nil {
		return err
	}
	if err := j.Deletions.Complete(ctx, sid, j.Clock.Now()); err != nil {
		return err
	}
	j.Logger.Info("session deleted", "session", sid)
	return nil
}

func (j *Janitor) interrupt(ctx context.Context, st *session.State) error {
	err := j.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		a := st.Active()
		if st.Created == nil || a == nil {
			return session.Decision{}, nil
		}
		return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_RunInterrupted{RunInterrupted: &v1.RunInterrupted{RunId: a.ID, By: "deletion"}}}}}, nil
	})
	if errors.Is(err, session.ErrGone) {
		return nil
	}
	return err
}

// forgetOnDevices 请求曾执行过该 Session 调用的设备清除本地记录。投影只保留最近 Run 的明细，因此扫描日志。
func (j *Janitor) forgetOnDevices(ctx context.Context, sid string, st *session.State) error {
	seen := map[string]bool{}
	var nodes []string
	err := session.ScanLog(ctx, j.Store.Log, sid, func(e *v1.Event) bool {
		if id := e.GetToolCallStarted().GetNodeId(); id != "" && !sandbox.IsNode(id) && id != askuser.NodeID && !seen[id] {
			seen[id] = true
			nodes = append(nodes, id)
		}
		return e.GetSeq() < st.Seq
	})
	if err != nil {
		return err
	}
	for _, id := range nodes {
		if _, err := j.Nodes.Get(ctx, id); errors.Is(err, node.ErrNotFound) {
			continue // Node 已随 EndUser 注销而删除
		} else if err != nil {
			return err
		}
		if err := j.Inbox.Put(ctx, id, ForgetInvoke(sid)); err != nil {
			return err
		}
	}
	return nil
}

// deleteChildren 删除委托派生的子 Session：子 Session ID 记在父日志的 ToolCallStarted 中（委托目标）。父 Session 按保留期
// 到期删除时，子 Session 同样按保留期删除：它们写入的 Memory 与父 Session 的一样保留。
func (j *Janitor) deleteChildren(ctx context.Context, sid string, st *session.State, reason string) error {
	var children []string
	err := session.ScanLog(ctx, j.Store.Log, sid, func(e *v1.Event) bool {
		if id := e.GetToolCallStarted().GetNodeId(); delegation.IsNode(id) && !slices.Contains(children, delegation.ChildSession(id)) {
			children = append(children, delegation.ChildSession(id))
		}
		return e.GetSeq() < st.Seq
	})
	if err != nil {
		return err
	}
	childReason := "parent_deleted"
	if reason == lifecycle.ReasonRetention {
		childReason = lifecycle.ReasonRetention
	}
	for _, child := range children {
		if err := j.Service.Delete(ctx, child, childReason, ""); err != nil && !errors.Is(err, service.ErrNotFound) {
			return err
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

// sweep 按登记顺序清除除日志与索引之外的全部数据；幂等，可重复执行。按保留期到期删除时跳过 KeepOnRetention 的数据。
func (j *Janitor) sweep(ctx context.Context, sid, reason string) error {
	for _, h := range j.Data {
		if h.KeepOnRetention && reason == lifecycle.ReasonRetention {
			continue
		}
		if err := h.DeleteSession(ctx, sid); err != nil {
			return fmt.Errorf("delete %s: %w", h.Name, err)
		}
	}
	return nil
}

// Sweep 执行保留策略（自动关闭与删除），重新入队未完成的删除（写入删除记录后、入队前崩溃的情况），
// 并按保留期清理沙箱账本。
func (j *Janitor) Sweep(ctx context.Context) error {
	j.init()
	now := j.Clock.Now()
	for bl, retention := range j.Retention {
		// 按 AgentDef 覆盖的规则各查各的 Session，业务线的值作用于其余 AgentDef（docs/design/captures.md §8）。
		for _, r := range retention.Rules() {
			if r.CloseAfterIdle > 0 {
				// 索引中的最后输入时间至多滞后 TouchEvery：多等这么久，不提前关闭。
				ids, err := j.Index.IdleBefore(ctx, bl, r.Agents, now.Add(-r.CloseAfterIdle-lifecycle.TouchEvery), 100)
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
				ids, err := j.Index.ClosedBefore(ctx, bl, r.Agents, now.Add(-r.DeleteAfterClose), 100)
				if err != nil {
					return err
				}
				for _, id := range ids {
					if err := j.Service.Delete(ctx, id, lifecycle.ReasonRetention, ""); err != nil && !errors.Is(err, service.ErrNotFound) {
						return err
					}
				}
			}
		}
	}
	pending, err := j.Deletions.Pending(ctx, 100)
	if err != nil {
		return err
	}
	for _, id := range pending {
		if err := j.Queue.Enqueue(ctx, id, workqueue.Class{Priority: workqueue.Background}); err != nil {
			return err
		}
	}
	if j.Presence != nil {
		if err := j.Presence.Prune(ctx, now); err != nil {
			return err
		}
	}
	if j.Usage != nil && j.UsageRetention > 0 {
		if err := j.Usage.Prune(ctx, now.Add(-j.UsageRetention)); err != nil {
			return err
		}
	}
	return j.Ledger.Prune(now.Add(-nodesdk.DefaultLedgerRetention))
}
