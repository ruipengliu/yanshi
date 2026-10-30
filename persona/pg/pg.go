// Package pg 管理 Persona 自己的连接与迁移；部署时必须使用独立于内核的数据库。
package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AcquireTimeout 只限制取得可用连接；正常 SQL、账本回调与锁排队不使用此短预算。
const AcquireTimeout = time.Second

// OperationTimeout 限制事务总时长，给正常并发下的锁等待及完整账本读写留出余量。
const OperationTimeout = 30 * time.Second

// StatementTimeout 同时用于事务内语句与行锁等待，不影响迁移连接。
const StatementTimeout = 15 * time.Second

// Operation 保留调用方更短的期限；提交结果未知时仍交给调用方按原 ID 对账。
func Operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, OperationTimeout)
}

// Acquire 将排队取连接与已开始的正常操作分开；取消等待不取消取得连接后的事务。
func Acquire(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	wait, cancel := context.WithTimeout(ctx, AcquireTimeout)
	defer cancel()
	return pool.Acquire(wait)
}

// Open 不执行迁移；部署方可用独立角色先调用 Migrate，再启动应用。
// schema 只用于测试隔离，空值沿用 DSN 的 search_path。
func Open(ctx context.Context, dsn, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// TCP 已连接但 PG 握手不应答时也必须结束；DSN 不能把运行时建连预算放大。
	if cfg.ConnConfig.ConnectTimeout == 0 || cfg.ConnConfig.ConnectTimeout > time.Second {
		cfg.ConnConfig.ConnectTimeout = time.Second
	}
	if schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize()
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := pool.Ping(probe); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Rollback 必须脱离请求取消，否则取消后的事务可能一直占用连接和 owner 锁。
func Rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
