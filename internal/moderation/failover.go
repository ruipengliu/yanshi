package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"yanshi/internal/metrics"
)

// Failover 是内容安全的容灾（延展性评审 §8.2）：依次询问提供商，只有前一个超时或出错时才改问下一个。
// 提供商须是故障域独立、经合规确认的等价提供商（上线检查清单）。第一个给出结论的提供商的结论即为结论：
// 主提供商拦截时不再询问备用方，备用方的放行因此不会覆盖主方的拦截。全部不可用时返回错误，调用方照旧不放行
// （ADR-0021）。
type Failover struct {
	// Names 与 Providers 一一对应，用于指标。
	Names     []string
	Providers []Moderator
	// Timeout 是每个提供商的时限；0 表示不单独限时（只受调用方 ctx 约束）。
	Timeout time.Duration
}

func (f Failover) Check(ctx context.Context, req Request) (Verdict, error) {
	var errs []error
	for i, p := range f.Providers {
		pctx, cancel := ctx, context.CancelFunc(func() {})
		if f.Timeout > 0 {
			pctx, cancel = context.WithTimeout(ctx, f.Timeout)
		}
		v, err := p.Check(pctx, req)
		cancel()
		if err == nil {
			return v, nil
		}
		if ctx.Err() != nil {
			return Verdict{}, ctx.Err()
		}
		name := fmt.Sprint(i)
		if i < len(f.Names) {
			name = f.Names[i]
		}
		metrics.ModerationFailovers.WithLabelValues(name).Inc()
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}
	return Verdict{}, errors.Join(errs...)
}
