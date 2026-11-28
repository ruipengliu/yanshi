package runtime

import (
	"context"
	"errors"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/metrics"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
	"yanshi/internal/workqueue"
)

func (w *Worker) drop() {
	w.endAttempt(tracing.Lost)
	w.lease, w.st, w.runID, w.attempt, w.modelErrors, w.forceCompact, w.failingSince = nil, nil, "", 0, 0, false, time.Time{}
	w.executed = nil
}

// upToDate 报告已知日志末尾没有超过本地投影，可以跳过本步的 Sync（压测中每个 Run 约 10 次读取）。
// 提示可能滞后：据此做出的决定若基于过期状态，追加时会冲突并重新同步，与没有提示时的竞争相同。
func (w *Worker) upToDate() bool {
	h, ok := w.cfg.Store.Log.(eventlog.HeadHinter)
	if !ok {
		return false
	}
	head, known := h.HeadHint(w.st.SessionID)
	return known && head <= w.st.Seq
}

// park 以 Session 当前的类别停放到 until，之后本 Worker 不再持有它。有未了结的后台调用时至多停放到最早的后台截止时间
// （ADR-0035）：活跃 Run 可能挂起很久（等审批），到期的后台调用不能跟着等。
func (w *Worker) park(ctx context.Context, until time.Time) error {
	if bg, ok := w.st.NextBackgroundDeadline(); ok && bg.Before(until) {
		until = bg
	}
	// 已到期的定时等当前 Run 结束，不能让挂起的 Run 在过去时刻反复被认领。
	if due, ok := w.st.NextScheduleDue(); ok && due.Before(until) && (w.st.Active() == nil || due.After(w.now())) {
		until = due
	}
	w.lease.Class = w.st.WorkClass()
	err := w.cfg.Queue.Park(ctx, w.lease, until)
	w.drop()
	if errors.Is(err, workqueue.ErrLeaseLost) {
		return nil
	}
	return err
}

// release 以 Session 当前的类别归还租约（done 见 workqueue.Queue.Release）。
func (w *Worker) release(ctx context.Context, done bool) error {
	if w.st != nil {
		w.lease.Class = w.st.WorkClass()
	}
	err := w.cfg.Queue.Release(ctx, w.lease, done)
	w.drop()
	if errors.Is(err, workqueue.ErrLeaseLost) {
		return nil
	}
	return err
}

// handOff 把 Run 移交给其他 Worker（ADR-0029）：持有 Attempt 时以 handoff 挂起，正常结束它，恢复不算接管；
// 否则直接归还租约。Session 立即可被认领，类别随之更新。
func (w *Worker) handOff(ctx context.Context, r *session.Run, reason string) error {
	metrics.Handoffs.WithLabelValues(reason).Inc()
	w.cfg.Logger.Info("run handed off", "worker", w.cfg.ID, "session", w.st.SessionID, "run", r.ID, "reason", reason)
	if !w.owns(r) {
		return w.release(ctx, false)
	}
	// 正在等待的调用已执行完、结果尚未写入：先写入结果，否则接手的 Worker 只能报告"结果未知"。只看当前等待的调用：
	// 缓存中可能还有结果其实已经写入（追加报告失败却已生效）的旧条目。
	if c := r.PendingCall(); c != nil {
		if res := w.executed[c.Call.GetCallId()]; res != nil {
			if err := w.commitExecuted(ctx, c.Call.GetCallId(), res); err != nil {
				return err
			}
			if w.lease == nil || !w.owns(r) {
				return nil // 写入时发现 Session 已删除或 Attempt 已失效
			}
		}
	}
	return w.suspendWith(ctx, &v1.RunSuspended{RunId: r.ID, Attempt: w.attempt, Reason: session.SuspendHandoff}, w.now())
}

// Stop 在停机时归还本 Worker 持有的工作：持有 Run 时移交它。之后 Worker 不再持有任何租约。
func (w *Worker) Stop(ctx context.Context) error {
	if w.lease == nil {
		return nil
	}
	// 先同步：移交依据的状态必须是最新的，否则可能为已经有结果的调用再写一次结果。同步失败时只归还租约。
	if w.st != nil && w.cfg.Store.Sync(ctx, w.st) == nil {
		w.observeAttempt()
		if r := w.st.Active(); r != nil {
			return w.handOff(ctx, r, "shutdown")
		}
	}
	return w.release(ctx, false)
}
