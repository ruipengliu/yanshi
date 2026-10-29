// Package pgtest 为 Persona 的 PG 测试提供独立 database 和逐测试 schema。
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"persona/pg"
)

const Database = "persona_test"

// New 仅把 YANSHI_TEST_PG 当作建库入口；Persona 表绝不写进内核的 database。
// 测试角色须有 CREATEDB 权限；各进程可共享测试库，schema 随测试清理。
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("YANSHI_TEST_PG")
	if dsn == "" {
		t.Skip("YANSHI_TEST_PG is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	createDatabase(t, ctx, cfg)
	cfg.Database = Database
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "persona_" + hex.EncodeToString(random[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	// 先登记 schema 清理，后登记 pool.Close；Cleanup 的逆序保证没有占用连接。
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		conn, err := pgx.ConnectConfig(cleanup, cfg)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close(cleanup) }()
		if _, err := conn.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.Database = Database
	poolCfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize()
	poolCfg.MaxConns = 4
	poolCfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pg.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func createDatabase(t *testing.T, ctx context.Context, cfg *pgx.ConnConfig) {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 直连的关闭也释放建库锁，失败路径不把会话锁留在池中。
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('persona_test_database'))`); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, Database).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{Database}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
}
