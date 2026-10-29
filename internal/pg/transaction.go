package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type transactionKey struct{}

type transactionScope struct {
	pool *pgxpool.Pool
	tx   pgx.Tx
}

// WithTx 让同一存储系统的清理回调沿用当前事务：锁住 EndUser 屏障期间不能另借连接，
// 否则小连接池会耗尽，清理与屏障删除也无法一起提交。调用方须保证事务内只有一个分片键。
func WithTx(ctx context.Context, pool *pgxpool.Pool, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, transactionKey{}, transactionScope{pool: pool, tx: tx})
}

// Exec 在匹配连接池的事务作用域中执行；普通调用仍使用连接池。
func Exec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := CheckPool(ctx, pool); err != nil {
		return pgconn.CommandTag{}, err
	}
	if scope, ok := ctx.Value(transactionKey{}).(transactionScope); ok {
		return scope.tx.Exec(ctx, sql, args...)
	}
	return pool.Exec(ctx, sql, args...)
}

// CheckPool 拒绝在已有事务中跨库调用；不能把事务的一部分静默独立提交。
func CheckPool(ctx context.Context, pool *pgxpool.Pool) error {
	if scope, ok := ctx.Value(transactionKey{}).(transactionScope); ok && scope.pool != pool {
		return fmt.Errorf("pg: transaction belongs to another pool")
	}
	return nil
}
