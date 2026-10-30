package day

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"yanshi/internal/pg/pgtest"
)

// 父测试池和子进程池各有旧 socket；生产 HTTP 恢复不能替父测试证明连接已刷新。
func TestDayObservationPoolReconnects(t *testing.T) {
	proxy, dsn := newTCPProxy(t, pgtest.DSN(t))
	pool, _ := freshBusinessDBAt(t, "yanshi_day_observer", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `CREATE TABLE observation_probe(id integer PRIMARY KEY); INSERT INTO observation_probe VALUES(1)`)
	must(t, err)
	proxy.cut(true)
	proxy.cut(false)
	e := &environment{t: t, ctx: ctx, personaPool: pool}
	e.observationPoolRecovered()
	var id int
	must(t, pool.QueryRow(ctx, `SELECT id FROM observation_probe`).Scan(&id))
	if id != 1 {
		t.Fatal("observation connection lost committed row")
	}
}

func TestDaySchemaCleanupAfterProxyRecovery(t *testing.T) {
	proxy, dsn := newTCPProxy(t, pgtest.DSN(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	must(t, err)
	defer conn.Close(context.Background())
	schema := "cleanup_" + randomID(t)
	_, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	must(t, err)
	proxy.cut(true)
	defer proxy.cut(false)
	result := make(chan error, 1)
	go func() { result <- dropDaySchema(ctx, dsn, schema) }()
	select {
	case err := <-result:
		t.Fatalf("cleanup did not wait for transient failure: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	proxy.cut(false)
	select {
	case err := <-result:
		must(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// 清理用新连接，不复用被故障切断的建 schema 连接。
	fresh, err := pgx.Connect(ctx, dsn)
	must(t, err)
	defer fresh.Close(context.Background())
	var exists bool
	must(t, fresh.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists))
	if exists {
		t.Fatal("schema survived cleanup")
	}
	must(t, dropDaySchema(ctx, dsn, schema))
}
