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

// 会话锁与进程一起消失；只占用连接，不把远程调用放进长事务。
// 同一 gate 内复用连接，避免小连接池中回调再次借连接而死锁。
func WithOwner(ctx context.Context, pool *pgxpool.Pool, line, user string, fn func(context.Context) error) error {
	if v, ok := ctx.Value(ownerKey{}).(ownerConn); ok && v.pool == pool {
		if v.line != line || v.user != user {
			return errors.New("xinli: cross-owner gate")
		}
		return fn(ctx)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	data, _ := json.Marshal([]string{"xinli_account", line, user})
	h := sha256.Sum256(data)
	key := int64(binary.BigEndian.Uint64(h[:8]))
	// 取消发生在锁已取得、结果尚未返回时也必须关闭连接，不能把持锁连接放回池。
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Conn().Close(cleanup)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, "SELECT pg_advisory_unlock($1)", key); err != nil {
			_ = conn.Conn().Close(cleanup)
		}
	}()
	return fn(context.WithValue(ctx, ownerKey{}, ownerConn{pool, conn, line, user}))
}
