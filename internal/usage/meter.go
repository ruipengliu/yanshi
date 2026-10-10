package usage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
)

// Meter 按价格表折算并记录用量。
//
// 记录失败时只记日志与指标：调用方（Worker、沙箱控制器）此时已经拿到结果，
// 让 Step 失败重试会再花一次钱，少计一次的代价更小。
type Meter struct {
	Store  Store
	Prices *PriceList
	// Deletions 非 nil 时，写入后检查 Session 的删除记录（先写、后查，ADR-0015）：Session 已被删除时
	// （不论原因）把这条用量匿名化。只看 account_deletion 不够：Session 可能先被单独删除，
	// 之后才注销账号，删除记录仍是原来的原因，而持有它的 Worker 可能在注销之后才写入（模拟测试发现）。
	Deletions lifecycle.Deletions
	Logger    *slog.Logger
}

// Scope 是用量的归属。SessionID 只用于删除检查，不随用量保存。
type Scope struct {
	SessionID    string
	BusinessLine string
	EndUser      string
}

// Model 记录一次模型调用（含上下文压缩）。id 是本次调用的唯一 ID，agent 是 name@version。
func (m *Meter) Model(ctx context.Context, s Scope, id, agent, model string, u *v1.Usage, at time.Time) {
	if m == nil || u == nil {
		return
	}
	m.record(ctx, s, &Entry{ID: id, BusinessLine: s.BusinessLine, EndUser: s.EndUser, Kind: Model, Model: model, Agent: agent,
		InputTokens: u.GetInputTokens(), CachedInputTokens: u.GetCachedInputTokens(), OutputTokens: u.GetOutputTokens(),
		Cost: m.Prices.ModelCost(model, u.GetInputTokens(), u.GetCachedInputTokens(), u.GetOutputTokens()), At: at})
}

// Call 记录 Call 中实时语音模型的一轮交互。id 是该轮的唯一 ID（Call ID 加轮次），agent 是 name@version。
func (m *Meter) Call(ctx context.Context, s Scope, id, agent, model string, t CallTokens, at time.Time) {
	if m == nil {
		return
	}
	m.record(ctx, s, &Entry{ID: id, BusinessLine: s.BusinessLine, EndUser: s.EndUser, Kind: Call, Model: model, Agent: agent,
		InputTokens: t.InputText + t.InputAudio, CachedInputTokens: min(t.CachedInput, t.InputText+t.InputAudio),
		OutputTokens: t.OutputText + t.OutputAudio, Cost: m.Prices.CallCost(model, t), At: at})
}

// Sandbox 记录一次沙箱执行。id 取自调用的 call_id：同一调用被重新投递时不重复计费。
func (m *Meter) Sandbox(ctx context.Context, s Scope, id string, d time.Duration, at time.Time) {
	if m == nil {
		return
	}
	m.record(ctx, s, &Entry{ID: id, BusinessLine: s.BusinessLine, EndUser: s.EndUser, Kind: Sandbox, Model: string(Sandbox),
		SandboxMillis: uint64(d.Milliseconds()), Cost: m.Prices.SandboxCost(d), At: at})
}

func (m *Meter) record(ctx context.Context, s Scope, e *Entry) {
	err := m.Store.Record(ctx, e)
	if err == nil && m.Deletions != nil && s.SessionID != "" {
		var t *lifecycle.Tombstone
		t, err = m.Deletions.Get(ctx, s.SessionID)
		switch {
		case errors.Is(err, lifecycle.ErrNotFound):
			err = nil
		case err == nil && t != nil:
			err = m.Store.AnonymizeEntry(ctx, e.ID)
		}
	}
	if err != nil {
		metrics.UsageRecordErrors.WithLabelValues().Inc()
		if m.Logger != nil {
			m.Logger.Warn("usage record failed", "session", s.SessionID, "usage", e.ID, "err", err)
		}
		return
	}
	metrics.UsageCost.WithLabelValues(e.BusinessLine, string(e.Kind)).Add(float64(e.Cost))
}
