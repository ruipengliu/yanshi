// Package pgtest 为测试提供隔离的 PostgreSQL schema。
//
// 需要设置 YANSHI_TEST_PG（make test 会启动 docker compose 依赖并设置它）；未设置时跳过。
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/pg"
)

// DSN 返回测试数据库地址；未配置时跳过测试。
func DSN(t testing.TB) string {
	dsn := os.Getenv("YANSHI_TEST_PG")
	if dsn == "" {
		t.Skip("YANSHI_TEST_PG not set; run `make test` (starts docker compose deps)")
	}
	return dsn
}

// Fresh 创建一个独立 schema、完成迁移，并启动 Notifier；测试结束时清理。
func Fresh(t testing.TB) (*pgxpool.Pool, *pg.Notifier) {
	t.Helper()
	dsn := DSN(t)
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "t_" + hex.EncodeToString(b[:])

	admin, err := pg.Open(ctx, dsn, "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	pool, err := pg.Open(ctx, dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	nctx, cancel := context.WithCancel(ctx)
	n := pg.NewNotifier(pool, nil)
	done := make(chan struct{})
	go func() { n.Run(nctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return pool, n
}
