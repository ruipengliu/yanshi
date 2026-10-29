// Package pgtest 为测试提供隔离的 PostgreSQL schema。
//
// 需要设置 YANSHI_TEST_PG（make test 会启动 docker compose 依赖并设置它）；未设置时跳过。
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	return FreshAt(t, DSN(t))
}

// FreshAt 在指定数据库中隔离 schema；双库测试的两个地址必须来自不同的 Database。
func FreshAt(t testing.TB, dsn string) (*pgxpool.Pool, *pg.Notifier) {
	t.Helper()
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

// Database 创建可清理的真实 database，不能用 schema 模拟事务与通知的库边界。
// Docker 的测试角色具有 CREATEDB；每组测试可在这两个数据库内继续用 FreshAt 隔离种子。
func Database(t testing.TB) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pg.Open(ctx, DSN(t), "")
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	name := "yanshi_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer admin.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	config, err := pgx.ParseConfig(DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	// 保留原地址的全部参数（含 TLS），只覆盖 database。
	dsn := config.ConnString()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		q := u.Query()
		q.Del("dbname")
		q.Del("database")
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " dbname=" + name
}

func FreshDatabase(t testing.TB) (*pgxpool.Pool, *pg.Notifier) {
	t.Helper()
	return FreshAt(t, Database(t))
}
