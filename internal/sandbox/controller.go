package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/node"
	"yanshi/internal/workqueue"
	"yanshi/sdk/nodesdk"
)

// Controller 是沙箱外的受信组件（ADR-0008）：认领沙箱、领取其 Inbox 中的调用、
// 在沙箱内执行并回写结果。与 Worker 一样以 Step 推进，以便确定性模拟。
type Controller struct {
	ID       string
	Queue    workqueue.Queue // 沙箱队列，键为沙箱 Node ID
	Hub      *node.Hub       // 提供 Inbox 与结果回写
	Provider Provider
	Activity Activity
	Ledger   nodesdk.Ledger // 共享存储，使任一控制器都能接管
	// Artifacts 用于工作区与工件之间的导入导出。
	Artifacts *artifact.Service
	// Lifecycle 非 nil 时，创建沙箱后复查 Session 是否已关闭或删除，是则销毁（ADR-0015）。
	Lifecycle *lifecycle.Guard
	Clock     clock.Clock
	Logger    *slog.Logger

	LeaseTTL time.Duration
	// Heartbeat > 0 时在执行期间续约（真实时间）；模拟测试置 0。
	Heartbeat time.Duration
	// IdleTTL 是沙箱空闲多久后回收计算资源。
	IdleTTL  time.Duration
	IdleWait time.Duration

	exec  *nodesdk.Executor
	lease *workqueue.Lease
}

func (c *Controller) init() {
	if c.exec == nil {
		c.exec = nodesdk.NewExecutor(c.Ledger, Capabilities(c.Provider, c.Artifacts)...)
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.IdleTTL == 0 {
		c.IdleTTL = 10 * time.Minute
	}
	if c.IdleWait == 0 {
		c.IdleWait = 100 * time.Millisecond
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

// Run 持续执行 Step，并周期性回收空闲沙箱，直到 ctx 结束。
func (c *Controller) Run(ctx context.Context) {
	c.init()
	lastReap := time.Time{}
	for ctx.Err() == nil {
		did, err := c.Step(ctx)
		if err != nil && ctx.Err() == nil {
			c.Logger.Warn("sandbox controller step failed", "controller", c.ID, "err", err)
		}
		if now := c.Clock.Now(); now.Sub(lastReap) > c.IdleTTL/4 {
			lastReap = now
			if err := c.Reap(ctx); err != nil && ctx.Err() == nil {
				c.Logger.Warn("sandbox reap failed", "err", err)
			}
		}
		if !did || err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(c.IdleWait):
			}
		}
	}
}

// Step 推进一件工作；返回 false 表示无事可做。
func (c *Controller) Step(ctx context.Context) (bool, error) {
	c.init()
	if c.lease == nil {
		l, err := c.Queue.Claim(ctx, c.ID, c.LeaseTTL)
		if errors.Is(err, workqueue.ErrEmpty) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		c.lease = l
	} else if err := c.Queue.Renew(ctx, c.lease, c.LeaseTTL); err != nil {
		c.lease = nil
		if errors.Is(err, workqueue.ErrLeaseLost) {
			return true, nil
		}
		return false, err
	}

	id := c.lease.SessionID
	pending, _, err := c.Hub.Inbox.Pending(ctx, id)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		err := c.Queue.Release(ctx, c.lease, true)
		c.lease = nil
		if errors.Is(err, workqueue.ErrLeaseLost) {
			err = nil
		}
		return true, err
	}
	inv := pending[0]

	if err := c.Activity.Touch(ctx, id, c.Clock.Now()); err != nil {
		return false, err
	}
	if err := c.Provider.Ensure(ctx, id); err != nil {
		return false, err
	}
	// 先创建、后复查：Session 已关闭或删除时销毁刚创建（或恢复）的沙箱，不执行调用（ADR-0015）。
	// 关闭方先记关闭、再让 Janitor 销毁沙箱，因此二者无论如何交错，工作区都不会残留。
	if gone, err := c.ended(ctx, inv.GetSessionId()); err != nil || gone {
		if gone {
			err = c.destroy(ctx, id)
			_ = c.Hub.Inbox.Remove(ctx, id, inv.GetCallId())
		}
		return false, err
	}
	res, err := c.during(ctx, id, inv.GetCallId(), func(ctx context.Context) (*v1.InvokeResult, error) {
		return c.exec.Execute(WithSandbox(ctx, id, inv.GetCallId()), inv)
	})
	if errors.Is(err, errWithdrawn) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := c.Activity.Touch(ctx, id, c.Clock.Now()); err != nil {
		return false, err
	}
	// 执行期间 Session 被删除：账本中刚写下的结果也要清除（先写、后查）。
	if gone, err := node.Withdrawn(ctx, c.Hub.Deletions, inv.GetSessionId()); err != nil || gone {
		if gone {
			err = c.Ledger.Forget(inv.GetSessionId())
		}
		return false, err
	}
	if res == nil {
		return true, nil // 同一调用正在本进程内执行
	}
	return true, c.Hub.Result(ctx, id, res)
}

var errWithdrawn = errors.New("call withdrawn")

// during 执行 fn；期间调用被撤回（中断）时取消 fn，配置了 Heartbeat 时持续续约。
func (c *Controller) during(ctx context.Context, id, callID string, fn func(context.Context) (*v1.InvokeResult, error)) (*v1.InvokeResult, error) {
	opCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		for {
			items, version, err := c.Hub.Inbox.Pending(opCtx, id)
			if err != nil {
				return
			}
			present := false
			for _, it := range items {
				present = present || it.GetCallId() == callID
			}
			if !present {
				cancel(errWithdrawn)
				return
			}
			if c.Hub.Inbox.Wait(opCtx, id, version) != nil {
				return
			}
		}
	}()
	if c.Heartbeat > 0 {
		lease := c.lease
		go func() {
			t := time.NewTicker(c.Heartbeat)
			defer t.Stop()
			for {
				select {
				case <-opCtx.Done():
					return
				case <-t.C:
					if err := c.Queue.Renew(opCtx, lease, c.LeaseTTL); err != nil {
						cancel(err)
						return
					}
				}
			}
		}()
	}
	res, err := fn(opCtx)
	if cause := context.Cause(opCtx); errors.Is(cause, errWithdrawn) {
		return nil, errWithdrawn
	}
	return res, err
}

// Reap 回收空闲沙箱的计算资源，工作区保留。
func (c *Controller) Reap(ctx context.Context) error {
	c.init()
	ids, err := c.Activity.ClaimIdle(ctx, c.Clock.Now().Add(-c.IdleTTL), 32)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.Provider.Stop(ctx, id); err != nil {
			return err
		}
		c.Logger.Info("sandbox stopped (idle)", "sandbox", id)
	}
	return nil
}

// Router 把路由调用分发给沙箱或设备：沙箱调用写入 Inbox 并唤醒沙箱队列，其余交给 Hub。
type Router struct {
	Hub   *node.Hub
	Queue workqueue.Queue // 沙箱队列
}

func (r *Router) Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke) error {
	if !IsNode(nodeID) {
		return r.Hub.Dispatch(ctx, nodeID, inv)
	}
	if err := r.Hub.Inbox.Put(ctx, nodeID, inv); err != nil {
		return err
	}
	if gone, err := node.Withdrawn(ctx, r.Hub.Deletions, inv.GetSessionId()); err != nil || gone {
		if gone {
			return r.Hub.Inbox.Remove(ctx, nodeID, inv.GetCallId())
		}
		return err
	}
	return r.Queue.Enqueue(ctx, nodeID)
}

func (r *Router) Cancel(ctx context.Context, nodeID, callID string) error {
	return r.Hub.Cancel(ctx, nodeID, callID)
}

func (c *Controller) destroy(ctx context.Context, id string) error {
	if err := c.Provider.Destroy(ctx, id); err != nil {
		return err
	}
	return c.Activity.Delete(ctx, id)
}

func (c *Controller) ended(ctx context.Context, sessionID string) (bool, error) {
	if c.Lifecycle != nil {
		return c.Lifecycle.Ended(ctx, sessionID)
	}
	return node.Withdrawn(ctx, c.Hub.Deletions, sessionID)
}
