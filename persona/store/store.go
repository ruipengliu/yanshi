// Package store 定义以 (BusinessLine, EndUser) 为键的事务接缝。
package store

import (
	"context"

	"persona/domain"
)

// Store 的读写事务只涉及一个键；PendingDeletions 只发现键，不跨键写入。Update 在隔离的 Book 上执行回调，成功才原子提交；
// 同键并发更新串行化。View 给出隔离的快照，回调的修改不会提交。
// 回调中的 Book 已绑定 domain.CaptureAccess，原文与账本使用同一快照/提交点。
// 回调不得重入同键操作；退出（含失败或 panic）即关闭原文接缝及迭代器，不能带到事务外使用。
// 账本引用离开回调后的修改不得影响存储；Call 的文字回执须同时检查 Call.Err 并作为回调错误返回。
type Store interface {
	Update(ctx context.Context, businessLine, endUser string, fn func(*domain.Book) error) error
	View(ctx context.Context, businessLine, endUser string, fn func(*domain.Book) error) error
	// PendingDeletions 只发现有删除待办或完成后仍需重新整理的所有者；P4 的 PG 实现按待办表索引查询，不扫描 Book。
	PendingDeletions(ctx context.Context) ([]Owner, error)
}

// Owner 是存储的隔离键，待办执行仍通过各键的 Update/View。
type Owner struct {
	BusinessLine string `json:"business_line"`
	EndUser      string `json:"end_user"`
}
