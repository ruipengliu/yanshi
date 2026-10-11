// Package eventlog 定义 Session Event 日志的存储契约（ADR-0004）。
//
// 每个 Session 一条只追加日志，seq 从 1 开始连续递增。实现必须通过
// eventlogtest.Run 的一致性测试。
package eventlog

import (
	"context"
	"errors"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/workqueue"
)

// ErrConflict 表示 Append 时日志末尾已不是 expectedSeq。
var ErrConflict = errors.New("eventlog: append conflict")

type Log interface {
	// Append 原子地追加 events，仅当日志末尾 seq 等于 expectedSeq 时成功。
	// 实现为每个事件设置 session_id 与 seq（会修改入参），返回新的末尾 seq。
	// 追加后事件视为不可变，调用方不得再修改。
	Append(ctx context.Context, sessionID string, expectedSeq uint64, events ...*v1.Event) (uint64, error)

	// Read 按顺序返回 seq > after 的事件，最多 limit 条；limit <= 0 表示不限。
	// 返回的事件不可修改。
	Read(ctx context.Context, sessionID string, after uint64, limit int) ([]*v1.Event, error)

	// Wait 阻塞直到日志末尾 seq > after，或 ctx 结束；返回当前末尾 seq。
	Wait(ctx context.Context, sessionID string, after uint64) (uint64, error)

	// Delete 物理删除整个 Session 的日志（ADR-0015）。只能在 Session 已有删除记录、
	// 不再有任何写入方之后调用；之后该 Session 视为空日志。
	Delete(ctx context.Context, sessionID string) error
}

// ReadAll 读取 seq > after 的全部事件。
func ReadAll(ctx context.Context, l Log, sessionID string, after uint64) ([]*v1.Event, error) {
	return l.Read(ctx, sessionID, after, 0)
}

// HeadHinter 是 Log 的可选接口：不访问存储就给出日志末尾 seq 的提示。提示可能滞后（新提交的通知尚未到达），
// 但不会超前；ok 为 false 表示不知道。调用方据此跳过不必要的读取，滞后由追加时的乐观并发冲突兜底。
type HeadHinter interface {
	HeadHint(sessionID string) (head uint64, ok bool)
}

// WakingLog 在追加的同一事务中把 Session 放入工作队列（延展性评审 §8.2）。外部写入（输入、审批、回答、
// Node 结果）之后必须有 Worker 推进 Session；追加与入队分两步时，二者之间崩溃会使 Run 停在原地，直到下一次输入。
//
// 实现只能把与自己同一存储的日志与队列组合起来（memlog 与 memqueue、pglog 与 pgqueue），
// 不能退回"先追加、再入队"两步：没有原子实现就不提供 WakingLog。实现必须通过 eventlogtest.RunWaking。
type WakingLog interface {
	// AppendAndEnqueue 与 Log.Append 相同，追加成功时在同一事务中以 class 入队（语义同 workqueue.Queue.Enqueue）：
	// 二者同时生效或同时不生效。冲突时返回 ErrConflict，队列不变。
	AppendAndEnqueue(ctx context.Context, sessionID string, expectedSeq uint64, class workqueue.Class, events ...*v1.Event) (uint64, error)
}
