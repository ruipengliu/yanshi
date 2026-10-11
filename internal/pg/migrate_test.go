package pg_test

import (
	"context"
	"slices"
	"testing"
	"testing/fstest"

	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
)

// TestMigrateFS：迁移可重复执行；标记 NoTransaction 的迁移在事务之外执行（CREATE INDEX CONCURRENTLY 在事务中会失败）；
// Pending 报告尚未应用的迁移。
func TestMigrateFS(t *testing.T) {
	ctx := context.Background()
	pool, _ := pgtest.Fresh(t) // 已应用全部内置迁移
	if pending, err := pg.Pending(ctx, pool); err != nil || len(pending) != 0 {
		t.Fatalf("pending after migrate = %v, %v", pending, err)
	}
	fsys := fstest.MapFS{
		"migrations/9001_table.sql": {Data: []byte("CREATE TABLE mig_t (id int);\nINSERT INTO mig_t VALUES (1);\n")},
		"migrations/9002_index.sql": {Data: []byte(pg.NoTransaction + "\nCREATE INDEX CONCURRENTLY IF NOT EXISTS mig_t_id ON mig_t (id);\n")},
	}
	pending, err := pg.PendingFS(ctx, pool, fsys)
	if err != nil || !slices.Equal(pending, []string{"migrations/9001_table.sql", "migrations/9002_index.sql"}) {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	for range 2 {
		if err := pg.MigrateFS(ctx, pool, fsys); err != nil {
			t.Fatal(err)
		}
	}
	var rows, valid int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mig_t`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("migration applied %d times (err %v)", rows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'mig_t_id' AND i.indisvalid`).Scan(&valid); err != nil || valid != 1 {
		t.Fatalf("concurrent index valid = %d (err %v)", valid, err)
	}
	if pending, err := pg.PendingFS(ctx, pool, fsys); err != nil || len(pending) != 0 {
		t.Fatalf("pending after migrate = %v, %v", pending, err)
	}
	// 失败的迁移整体回滚，不记录版本。
	bad := fstest.MapFS{"migrations/9003_bad.sql": {Data: []byte("CREATE TABLE mig_u (id int);\nSELECT no_such_function();\n")}}
	if err := pg.MigrateFS(ctx, pool, bad); err == nil {
		t.Fatal("broken migration succeeded")
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('mig_u') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("partial migration left behind (exists %v, err %v)", exists, err)
	}
	if pending, _ := pg.PendingFS(ctx, pool, bad); len(pending) != 1 {
		t.Fatalf("failed migration recorded as applied: pending %v", pending)
	}
}
