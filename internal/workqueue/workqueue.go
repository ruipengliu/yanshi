// Package workqueue 定义按 Session 发放租约的工作队列。
//
// 队列只负责活性（让某个 Worker 去推进某个 Session），不负责正确性：
// 正确性由 Event 日志的 Attempt fencing 保证（docs/design/m0-core-primitives.md §3）。
package workqueue

import (
	"context"
	"errors"
	"slices"
	"time"
)

var (
	// ErrEmpty 表示当前没有可认领的 Session。
	ErrEmpty = errors.New("workqueue: empty")
	// ErrLeaseLost 表示租约已过期并可能已被他人认领。
	ErrLeaseLost = errors.New("workqueue: lease lost")
)

// Priority 是工作的优先级（ADR-0029）：同一队列中先认领优先级高的工作，同一优先级内按入队顺序。
type Priority int16

const (
	// Background 是长任务：活跃 Run 调用模型已达 LongRunTurns 次，多半没有人盯着等。
	Background Priority = 0
	// Interactive 是有人在等的工作：新输入、问答、刚开始的 Run。
	Interactive Priority = 1
)

// LongRunTurns 是 Run 调用模型多少次之后被视为长任务。按已做的工作而不是经过的时间判定：
// 积压时排队久了的新 Run 不应被降级，从挂起恢复的 Run（如用户刚做出审批）也不应因此降级。
const LongRunTurns = 6

// Class 是工作的类别：所属业务线与优先级。
type Class struct {
	BusinessLine string
	Priority     Priority
}

// Classify 返回推进一个 Session 的工作的类别；turns 是活跃 Run 已调用模型的次数，没有活跃 Run 时为 0。
func Classify(businessLine string, turns int) Class {
	p := Interactive
	if turns >= LongRunTurns {
		p = Background
	}
	return Class{BusinessLine: businessLine, Priority: p}
}

// Pool 选择认领哪些类别的工作；零值认领任意工作。
type Pool struct {
	// BusinessLines 非空时只认领这些业务线的工作。
	BusinessLines []string
	// MinPriority 是认领的最低优先级。
	MinPriority Priority
}

// Admits 报告 c 类的工作是否属于本池。
func (p Pool) Admits(c Class) bool {
	return c.Priority >= p.MinPriority && (len(p.BusinessLines) == 0 || slices.Contains(p.BusinessLines, c.BusinessLine))
}

type Lease struct {
	SessionID string
	// Class 是认领时工作的类别。持有者可以改写它：Park 与 Release 以它更新类别。
	Class  Class
	Holder string
	// Token 在每次认领时变化，用于识别租约是否仍属于持有者。
	Token   uint64
	Expires time.Time
}

type Queue interface {
	// Remove 无条件移除键（包括已租出的）：持有者随后的续约与释放得到 ErrLeaseLost。
	// 用于 Session 删除（ADR-0015）。
	Remove(ctx context.Context, sessionID string) error
	// Enqueue 标记 Session 需要推进，并设置其类别（已在队列中时改用新类别：最近一次入队最了解当前情况）；
	// 幂等。对已被租出的 Session 调用，会使其在 Release(done=true) 或 Park 后仍立即可认领；
	// 对已 Park 的 Session 调用会唤醒它。
	Enqueue(ctx context.Context, sessionID string, class Class) error

	// Claim 在 pool 中认领一个可用或租约已过期的 Session：优先级高的先认领，同一优先级内按入队顺序。
	// 无可认领时返回 ErrEmpty。
	Claim(ctx context.Context, holder string, ttl time.Duration, pool Pool) (*Lease, error)

	// Renew 延长租约；租约已失效时返回 ErrLeaseLost。
	Renew(ctx context.Context, lease *Lease, ttl time.Duration) error

	// Park 归还租约，并使 Session 在 until 之前不可认领，除非期间被 Enqueue 唤醒
	// （租约期间发生的 Enqueue 同样生效，此时 Session 立即可认领）。类别更新为 lease.Class。
	Park(ctx context.Context, lease *Lease, until time.Time) error

	// Release 归还租约。done=true 表示 Session 已无待办工作，从队列移除
	// （除非认领后又被 Enqueue）；done=false 表示立即可被他人认领。类别更新为 lease.Class。
	Release(ctx context.Context, lease *Lease, done bool) error
}

// Signaler 是 Queue 的可选接口：Ready 返回的通道在可能有新工作入队时收到信号。信号只是提示，
// 可能合并或丢失；挂起到期与租约过期也不会发信号，因此等待者仍须定时轮询兜底。
type Signaler interface {
	Ready() <-chan struct{}
}
