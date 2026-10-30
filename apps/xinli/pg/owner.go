package pg

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type ownerKey struct{}
type ownerConn struct {
	pool       *pgxpool.Pool
	conn       *pgxpool.Conn
	line, user string
}

func Executor(ctx context.Context, pool *pgxpool.Pool) DB {
	if v, ok := ctx.Value(ownerKey{}).(ownerConn); ok && v.pool == pool {
		return v.conn
	}
	return pool
}

// WithConnection 给鉴权前的查询单独限制借连接；owner gate 内复用已有连接，避免自锁。
func WithConnection(ctx context.Context, pool *pgxpool.Pool, fn func(DB) error) error {
	if value, ok := ctx.Value(ownerKey{}).(ownerConn); ok && value.pool == pool {
		return fn(value.conn)
	}
	wait, cancel := context.WithTimeout(ctx, time.Second)
	conn, err := pool.Acquire(wait)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Release()
	return fn(conn)
}

// 会话锁与进程一起消失；只占用连接，不把远程调用放进长事务。
// 同一 gate 内复用连接，避免小连接池中回调再次借连接而死锁。
func WithOwner(ctx context.Context, pool *pgxpool.Pool, line, user string, fn func(context.Context) error) error {
	if v, ok := ctx.Value(ownerKey{}).(ownerConn); ok && v.pool == pool {
		if v.line != line || v.user != user {
			return errors.New("xinli: cross-owner gate")
		}
		return fn(ctx)
	}
	// 借连接失败快速返回，健康连接上的 owner 排队使用宽裕预算；回调沿用父 context。
	acquire, acquired := context.WithTimeout(ctx, time.Second)
	conn, err := pool.Acquire(acquire)
	acquired()
	if err != nil {
		return err
	}
	defer conn.Release()
	wait, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	data, _ := json.Marshal([]string{"xinli_account", line, user})
	h := sha256.Sum256(data)
	key := int64(binary.BigEndian.Uint64(h[:8]))
	// 取消发生在锁已取得、结果尚未返回时也必须关闭连接，不能把持锁连接放回池。
	if _, err = conn.Exec(wait, "SELECT pg_advisory_lock($1)", key); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		_ = conn.Conn().Close(cleanup)
		if wait.Err() != nil {
			return wait.Err()
		}
		return err
	}
	stop()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		if _, err := conn.Exec(cleanup, "SELECT pg_advisory_unlock($1)", key); err != nil {
			_ = conn.Conn().Close(cleanup)
		}
	}()
	return fn(context.WithValue(ctx, ownerKey{}, ownerConn{pool, conn, line, user}))
}
