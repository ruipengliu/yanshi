package service

import (
	"context"

	"yanshi/internal/lifecycle"
)

// DeletionRequest 先用控制库中的业务线授权，再汇总进度，避免越权查询触发其他分片。
func (s *Service) DeletionRequest(ctx context.Context, businessLine, id string) (*lifecycle.Request, error) {
	if progress, ok := s.Deletions.(lifecycle.RequestProgress); ok {
		r, err := progress.RequestInfo(ctx, id)
		if err != nil {
			return nil, err
		}
		if r.BusinessLine != businessLine {
			return nil, lifecycle.ErrNotFound
		}
		r.Sessions, r.Completed, err = progress.CountRequest(ctx, id)
		if err != nil {
			return nil, err
		}
		return r, nil
	}
	// 保留未提供分片进度接口的单库替身；生产装配的所有实现都走上面的先授权路径。
	r, err := s.Deletions.Request(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.BusinessLine != businessLine {
		return nil, lifecycle.ErrNotFound
	}
	return r, nil
}
