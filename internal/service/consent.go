package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"yanshi/internal/consent"
	"yanshi/internal/lifecycle"
)

// ConsentStatus 是 EndUser 在业务线中一类信息的单独同意。GrantedAt 是平台侧可出示的同意记录：业务线登记同意的时间，
// 重复登记保留最初的时间；撤回后为空。
type ConsentStatus struct {
	Kind      consent.Kind `json:"kind"`
	GrantedAt *time.Time   `json:"granted_at,omitempty"`
	// Purged 是撤回时一并删除的数据条数，只在请求清除时出现（DELETE ?purge=true）。
	Purged *int `json:"purged,omitempty"`
}

func (s *Service) checkConsent(bl, eu string, kind consent.Kind) error {
	if bl == "" || eu == "" {
		return fmt.Errorf("%w: business_line and end_user are required", ErrInvalid)
	}
	if !consent.Valid(kind) {
		return fmt.Errorf("%w: unknown consent kind %q", ErrInvalid, kind)
	}
	if s.Consents == nil || s.Deletions == nil {
		return fmt.Errorf("%w: consents or deletion protection are not configured", ErrInvalid)
	}
	return nil
}

// PutConsent 登记 EndUser 对一类信息的单独同意（ADR-0022 修订）：业务线在自己的产品中取得同意之后调用。
// 业务线须已开启该类信息（memory.health），否则登记没有效果。与 PutTier 相同，先查、后复查 EndUser 删除屏障；
// 即使写入回包丢失，也撤回可能已落盘的记录。
func (s *Service) PutConsent(ctx context.Context, bl, eu string, kind consent.Kind) (*ConsentStatus, error) {
	if err := s.checkConsent(bl, eu, kind); err != nil {
		return nil, err
	}
	if kind == consent.Health && (s.HealthEnabled == nil || !s.HealthEnabled(bl)) {
		return nil, fmt.Errorf("%w: health information is not enabled for this business line (memory.health); a consent would have no effect", ErrInvalid)
	}
	deleted, err := lifecycle.EndUserDeleted(ctx, s.Deletions, bl, eu)
	if err != nil {
		return nil, err
	}
	if deleted {
		return nil, ErrNotFound
	}
	writeErr := s.Consents.Put(ctx, bl, eu, kind, s.Store.Clock.Now())
	// 请求取消不代表没有提交；复查也不能依赖请求仍然存活。
	checkCtx := context.WithoutCancel(ctx)
	withdraw := func(ctx context.Context, _ *lifecycle.EndUserTombstone) error {
		return s.Consents.DeleteEndUser(ctx, bl, eu)
	}
	deleted, checkErr := lifecycle.RecheckEndUser(checkCtx, s.Deletions, bl, eu, withdraw)
	if checkErr != nil {
		// 无法确认屏障时保守撤回，注销重试必须能够继续清理。
		return nil, errors.Join(writeErr, checkErr, s.Consents.Delete(checkCtx, bl, eu, kind))
	}
	if deleted {
		return nil, errors.Join(ErrNotFound, writeErr)
	}
	if writeErr != nil {
		return nil, writeErr
	}
	at, err := s.Consents.Get(ctx, bl, eu, kind)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		// 与同时到达的撤回交错：以撤回为准，调用方重试即可。
		return nil, fmt.Errorf("%w: consent was withdrawn concurrently", ErrConflict)
	}
	return &ConsentStatus{Kind: kind, GrantedAt: &at}, nil
}

// DeleteConsent 撤回 EndUser 对一类信息的单独同意：写入、召回、检索与经 Grant 的读取立即停止（本进程立即，其他进程至多
// consent.CacheTTL）。已有的数据不在这里删除，由调用方决定是否清除（ADR-0022 修订第 3 条）。业务线未开启时也允许撤回。
func (s *Service) DeleteConsent(ctx context.Context, bl, eu string, kind consent.Kind) (*ConsentStatus, error) {
	if err := s.checkConsent(bl, eu, kind); err != nil {
		return nil, err
	}
	if err := s.Consents.Delete(ctx, bl, eu, kind); err != nil {
		return nil, err
	}
	return &ConsentStatus{Kind: kind}, nil
}
