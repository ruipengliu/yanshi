package runtime

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/metrics"
	"yanshi/internal/session"
)

// awaitModeration 处理输出审核不可用（ADR-0054）：丢弃尚未写入日志的回复，以不含正文的 RunSuspended 挂起，
// 退避到期后重新生成并送检。被丢弃的那次模型调用已经计量（ADR-0019），退避使故障期间不再每步重新调用模型。
// 从第一次挂起起累计超过 ModerationWaitMax 时 Run 失败：不放行的原则不变。
func (w *Worker) awaitModeration(ctx context.Context, r *session.Run) error {
	now := w.now()
	if r.ModerationWaits > 0 && now.Sub(r.ModerationWaitSince) >= w.cfg.ModerationWaitMax {
		metrics.ModerationWaits.WithLabelValues("failed").Inc()
		w.cfg.Logger.Warn("run failed: moderation unavailable", "session", w.st.SessionID, "run", r.ID, "waits", r.ModerationWaits)
		return w.fail(ctx, r, "moderation unavailable")
	}
	until := now.Add(w.moderationBackoff(r.ModerationWaits))
	metrics.ModerationWaits.WithLabelValues("suspended").Inc()
	w.cfg.Logger.Warn("run suspended: moderation unavailable", "session", w.st.SessionID, "run", r.ID, "waits", r.ModerationWaits+1)
	return w.suspendWith(ctx, &v1.RunSuspended{RunId: r.ID, Attempt: w.attempt, Reason: session.SuspendModeration, Until: timestamppb.New(until)}, until)
}

// moderationBackoff 是第 waits+1 次挂起的等待时长：ModerationRetry 起每次翻倍，至多 ModerationRetryMax。
func (w *Worker) moderationBackoff(waits int) time.Duration {
	d := w.cfg.ModerationRetry
	for i := 0; i < waits && d < w.cfg.ModerationRetryMax; i++ {
		d *= 2
	}
	return min(d, w.cfg.ModerationRetryMax)
}
