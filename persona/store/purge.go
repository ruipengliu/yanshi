package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrOwnerDeleting      = errors.New("persona: owner deletion barrier is active")
	ErrDeletionNotStarted = errors.New("persona: owner deletion has not started")
	ErrPurgeLimit         = errors.New("persona: purge limit must be between 1 and 1000")
	ErrDeletionTime       = errors.New("persona: deletion time is required")
)

const MaxPurgeBatch = 1000

// OwnerDeletion 只含键、状态和时间；CompletedAt 只证明 Persona 已清空，不代表账号逐库注销完成。
// Q104 的重新注册使用新 EndUser ID，旧屏障永久保留，不设宽限期或重置入口。
type OwnerDeletion struct {
	Owner
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

// Purger 是可注入时钟的单轮驱动；应用负责周期、重试与生命周期，A11 接生产装配。
// 一轮每个 owner 只做一批，失败保留发现条件；重新创建驱动即可继续，不保存进程内游标。
type Purger struct {
	Store     Store
	Now       func() time.Time
	BatchSize int
}

func (p Purger) Scan(ctx context.Context) error {
	limit := p.BatchSize
	if limit == 0 {
		limit = MaxPurgeBatch
	}
	if limit < 1 || limit > MaxPurgeBatch {
		return ErrPurgeLimit
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	owners, err := p.Store.PendingPurges(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		_, err := p.Store.Purge(ctx, owner.BusinessLine, owner.EndUser, limit, now())
		result = errors.Join(result, err)
	}
	return result
}
