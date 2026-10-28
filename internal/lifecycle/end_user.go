package lifecycle

import (
	"context"
	"errors"
	"time"
)

// DefaultEndUserGrace 拦住注销前已在途、清除完成后才落盘的写入；业务线仍须先使用户令牌失效。
const DefaultEndUserGrace = 24 * time.Hour

// EndUserTombstone 在注销执行期与完成后的宽限期内保存 EndUser，期满后由 Janitor 删除。
// Sessions 只记待清理的 Session ID，避免跨 EndUser 与 Session 键事务；逐个确认删除完成后才开始宽限期。
type EndUserTombstone struct {
	BusinessLine string
	EndUser      string
	RequestID    string
	RequestedAt  time.Time
	Sealed       bool
	Sessions     []string
	Revision     int64
	CompletedAt  time.Time
	ExpiresAt    time.Time
}

// EndUserDeletions 将屏障保留到清除完成后的宽限期结束，Session 删除记录仍独立保存。
type EndUserDeletions interface {
	// MarkEndUser 先写屏障，已存在时返回原记录（重试复用原删除请求）。
	MarkEndUser(context.Context, *EndUserTombstone) (*EndUserTombstone, error)
	GetEndUser(ctx context.Context, businessLine, endUser string) (*EndUserTombstone, error)
	// AddEndUserSessions 合并待清理 ID；新 ID 使完成状态失效，重复 ID 不改变宽限期。
	AddEndUserSessions(context.Context, *EndUserTombstone, []string) error
	// SealEndUser 在列举、登记 Session 删除与所有 EndUser 清除均成功后调用。
	SealEndUser(context.Context, *EndUserTombstone) error
	// PendingEndUsers 只返回已 Seal、尚未完成的屏障，供 Janitor 逐 Session 确认清理进度。
	PendingEndUsers(ctx context.Context, limit int) ([]*EndUserTombstone, error)
	// CompleteEndUser 仅在已 Seal、Revision 未变且尚未完成时记录完成时间与宽限期终点。
	CompleteEndUser(ctx context.Context, b *EndUserTombstone, at, expires time.Time) error
	// PruneEndUsers 逐 EndUser 复核到期条件，再清理、再删除屏障；清理失败保留该屏障并继续其他候选。
	// cleanup 不得重入 Deletions；PG 清理须经 pg.Exec 沿用同键事务。nil 只用于没有附属数据的存储测试。
	PruneEndUsers(ctx context.Context, now time.Time, limit int, cleanup func(context.Context, string, string) error) error
}

// EndUserDeleted 报告该业务线的 EndUser 屏障是否仍在（执行期或宽限期）。
func EndUserDeleted(ctx context.Context, d EndUserDeletions, businessLine, endUser string) (bool, error) {
	if d == nil || endUser == "" {
		return false, nil
	}
	_, err := d.GetEndUser(ctx, businessLine, endUser)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// RecheckEndUser 是按 EndUser 写入的“先写、后查”：查到屏障后用不随请求取消的上下文撤回。
// withdraw 得到屏障中的请求 ID，撤回的新 Session 因此与原注销一起等待清理完成。
func RecheckEndUser(ctx context.Context, d EndUserDeletions, businessLine, endUser string, withdraw func(context.Context, *EndUserTombstone) error) (bool, error) {
	if d == nil || endUser == "" {
		return false, nil
	}
	b, err := d.GetEndUser(ctx, businessLine, endUser)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, withdraw(context.WithoutCancel(ctx), b)
}

// EndUserHolder 是注销时需要清理的一处按 EndUser 保存的数据；到期屏障删除前再清一遍，
// 兜住写入已提交、复查之前进程崩溃的窗口。登记由 app.NewCore 的对象图提供。
type EndUserHolder struct {
	Name   string
	Delete func(ctx context.Context, businessLine, endUser string) error
	// Invalidate 在清理事务结束后失效缓存；可为 nil。
	Invalidate func(businessLine, endUser string)
}
