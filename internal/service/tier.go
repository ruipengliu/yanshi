package service

import (
	"context"
	"errors"
	"fmt"

	"yanshi/internal/lifecycle"
	"yanshi/internal/usage"
)

// TierStatus 是有效档位与日限额；空档位表示默认，零限额表示不限。
type TierStatus struct {
	Tier  string `json:"tier"`
	Limit int64  `json:"limit_micros"`
}

func (s *Service) tierLimits(bl, eu string) (usage.Limits, error) {
	if bl == "" || eu == "" {
		return usage.Limits{}, fmt.Errorf("%w: business_line and end_user are required", ErrInvalid)
	}
	if !s.Quotas.Enabled(bl) {
		return usage.Limits{}, fmt.Errorf("%w: quotas are disabled for this business line; a tier would have no effect", ErrInvalid)
	}
	if s.Tiers == nil || s.Deletions == nil {
		return usage.Limits{}, fmt.Errorf("%w: tiers or deletion protection are not configured", ErrInvalid)
	}
	return s.Quotas.Limits[bl], nil
}

// PutTier 先查、后复查屏障；即使写入回包丢失，也撤回可能已落盘的档位。
func (s *Service) PutTier(ctx context.Context, bl, eu, tier string) (*TierStatus, error) {
	limits, err := s.tierLimits(bl, eu)
	if err != nil {
		return nil, err
	}
	l, ok := limits.Tiers[tier]
	if !ok {
		return nil, fmt.Errorf("%w: unknown tier", ErrInvalid)
	}
	deleted, err := lifecycle.EndUserDeleted(ctx, s.Deletions, bl, eu)
	if err != nil {
		return nil, err
	}
	if deleted {
		return nil, ErrNotFound
	}
	writeErr := s.Tiers.Put(ctx, bl, eu, tier, s.Store.Clock.Now())
	// 请求取消不代表没有提交；复查也不能依赖请求仍然存活。
	checkCtx := context.WithoutCancel(ctx)
	withdraw := func(ctx context.Context, _ *lifecycle.EndUserTombstone) error {
		return s.Tiers.Delete(ctx, bl, eu)
	}
	deleted, checkErr := lifecycle.RecheckEndUser(checkCtx, s.Deletions, bl, eu, withdraw)
	if checkErr != nil {
		// 无法确认屏障时保守撤回，注销重试必须能够继续清理。
		return nil, errors.Join(writeErr, checkErr, s.Tiers.Delete(checkCtx, bl, eu))
	}
	if deleted {
		return nil, errors.Join(ErrNotFound, writeErr)
	}
	if writeErr != nil {
		return nil, writeErr
	}
	return &TierStatus{Tier: tier, Limit: usage.Yuan(l.EndUserDaily)}, nil
}

func (s *Service) DeleteTier(ctx context.Context, bl, eu string) (*TierStatus, error) {
	limits, err := s.tierLimits(bl, eu)
	if err != nil {
		return nil, err
	}
	if err := s.Tiers.Delete(ctx, bl, eu); err != nil {
		return nil, err
	}
	return &TierStatus{Limit: usage.Yuan(limits.EndUserDaily)}, nil
}
