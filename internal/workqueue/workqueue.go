// Package workqueue 定义按 Session 发放租约的工作队列。
//
// 队列只负责活性（让某个 Worker 去推进某个 Session），不负责正确性：
// 正确性由 Event 日志的 Attempt fencing 保证（docs/design/m0-core-primitives.md §3）。
package workqueue

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrEmpty 表示当前没有可认领的 Session。
	ErrEmpty = errors.New("workqueue: empty")
	// ErrLeaseLost 表示租约已过期并可能已被他人认领。
	ErrLeaseLost = errors.New("workqueue: lease lost")
)

type Lease struct {
	SessionID string
	Holder    string
	// Token 在每次认领时变化，用于识别租约是否仍属于持有者。
	Token   uint64
	Expires time.Time
}

type Queue interface {
	// Enqueue 标记 Session 需要推进；幂等。对已被租出的 Session 调用，
	// 会使其在 Release(done=true) 或 Park 后仍立即可认领；对已 Park 的 Session 调用会唤醒它。
	Enqueue(ctx context.Context, sessionID string) error

	// Claim 认领一个可用或租约已过期的 Session。无可认领时返回 ErrEmpty。
	Claim(ctx context.Context, holder string, ttl time.Duration) (*Lease, error)

	// Renew 延长租约；租约已失效时返回 ErrLeaseLost。
	Renew(ctx context.Context, lease *Lease, ttl time.Duration) error

	// Park 归还租约，并使 Session 在 until 之前不可认领，除非期间被 Enqueue 唤醒
	// （租约期间发生的 Enqueue 同样生效，此时 Session 立即可认领）。
	Park(ctx context.Context, lease *Lease, until time.Time) error

	// Release 归还租约。done=true 表示 Session 已无待办工作，从队列移除
	// （除非认领后又被 Enqueue）；done=false 表示立即可被他人认领。
	Release(ctx context.Context, lease *Lease, done bool) error
}

// Signaler 是 Queue 的可选接口：Ready 返回的通道在可能有新工作入队时收到信号。信号只是提示，
// 可能合并或丢失；挂起到期与租约过期也不会发信号，因此等待者仍须定时轮询兜底。
type Signaler interface {
	Ready() <-chan struct{}
}
