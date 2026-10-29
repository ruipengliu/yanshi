package pg_test

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"persona/pg"
	"persona/pg/pgtest"
)

func TestMigrate(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	for range 2 {
		if err := pg.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var database string
	var count int
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if database != pgtest.Database {
		t.Fatalf("not a dedicated database: %s", database)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM persona_schema_migrations`).Scan(&count); err != nil || count != 3 {
		t.Fatal("migration was not recorded once", count, err)
	}
}

func TestMigrationRollbackAndConcurrentIndex(t *testing.T) {
	pool := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	broken := fstest.MapFS{"migrations/0004_broken.sql": {Data: []byte(`CREATE TABLE migration_probe (id int); SELECT 1/0;`)}}
	if err := pg.MigrateFS(ctx, pool, broken); err == nil {
		t.Fatal("broken migration succeeded")
	}
	var absent bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('migration_probe') IS NULL
		AND NOT EXISTS (SELECT 1 FROM persona_schema_migrations WHERE version=4)`).Scan(&absent); err != nil || !absent {
		t.Fatal("migration did not roll back", err)
	}
	files := fstest.MapFS{
		"migrations/0004_probe.sql": {Data: []byte(`CREATE TABLE migration_probe (id int);`)},
		"migrations/0005_index.sql": {Data: []byte(pg.NoTransaction + "\nCREATE INDEX CONCURRENTLY IF NOT EXISTS migration_probe_idx ON migration_probe(id);")},
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- pg.MigrateFS(ctx, pool, files) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	// 模拟非事务 SQL 成功、记录版本之前退出；可重复执行不能破坏索引。
	if _, err := pool.Exec(ctx, `DELETE FROM persona_schema_migrations WHERE version=5`); err != nil {
		t.Fatal(err)
	}
	if err := pg.MigrateFS(ctx, pool, files); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid='migration_probe_idx'::regclass`).Scan(&valid); err != nil || !valid {
		t.Fatal("index not valid", err)
	}
}
