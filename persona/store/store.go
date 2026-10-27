// Package store 定义以 (BusinessLine, EndUser) 为键的事务接缝。
package store

import (
	"context"
	"persona/domain"
)

// Store 的一次调用只涉及一个键。Update 在隔离的 Book 上执行回调，成功才原子提交；
// 同键并发更新串行化。View 给出隔离的快照，回调的修改不会提交。
// 回调不得重入同键的存储操作；离开回调后继续修改持有的引用也不得影响存储。
type Store interface {
	Update(ctx context.Context, businessLine, endUser string, fn func(*domain.Book) error) error
	View(ctx context.Context, businessLine, endUser string, fn func(*domain.Book) error) error
}
