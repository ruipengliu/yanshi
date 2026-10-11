package runtime

import (
	"context"
	"time"

	"yanshi/internal/metrics"
	"yanshi/internal/usage"
)

func (w *Worker) usageScope() usage.Scope {
	return usage.Scope{SessionID: w.st.SessionID, BusinessLine: w.st.Created.GetBusinessLine(), EndUser: w.st.Created.GetEndUser()}
}

// overQuota 返回当前 Session 所属业务线或 EndUser 已超出的一层配额；未配置配额时返回 nil。
// where 非空时计入拒绝指标（复查不计，否则长期超额的 Run 会持续累加）。
func (w *Worker) overQuota(ctx context.Context, where string) (*usage.Period, error) {
	bl := w.st.Created.GetBusinessLine()
	if !w.cfg.Quotas.Enabled(bl) {
		return nil, nil
	}
	p, err := w.cfg.Quotas.Check(ctx, bl, w.st.Created.GetEndUser(), w.now())
	if p != nil && where != "" {
		metrics.QuotaRejections.WithLabelValues(p.Scope, where).Inc()
	}
	return p, err
}

// quotaPark 是因配额挂起时的停放时间：配额重置时，或更早的定期复查（调高配额后及时恢复）。
func (w *Worker) quotaPark(p *usage.Period) time.Time {
	if t := w.now().Add(w.cfg.QuotaRecheck); t.Before(p.ResetAt) {
		return t
	}
	return p.ResetAt
}

// reparkOverQuota 处理因配额挂起后被唤醒的 Run：仍超额时直接重新停放，不开始 Attempt、不写事件，
// 避免长期超额的 Run 每次复查都向日志写入 AttemptStarted 与 RunSuspended（ADR-0019）。
func (w *Worker) reparkOverQuota(ctx context.Context) (bool, error) {
	p, err := w.overQuota(ctx, "")
	if err != nil || p == nil {
		return false, err
	}
	return true, w.park(ctx, w.quotaPark(p))
}
