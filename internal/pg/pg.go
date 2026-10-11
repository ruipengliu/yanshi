// Package pg 是 PostgreSQL 存储实现的公共部分：连接池、迁移与 LISTEN/NOTIFY 通知（ADR-0007）。
package pg

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open 创建连接池；schema 非空时所有连接使用该 schema（测试隔离用）。
func Open(ctx context.Context, dsn, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if schema != "" {
		// public 在后：扩展（如 pgvector）安装在 public 中，所有 schema 共用。
		cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// MigrateLock 是迁移所持会话级 advisory lock 的键（hashtext）。锁是全库范围的：迁移会创建扩展
// （CREATE EXTENSION 是库级操作），并发创建会冲突。
const MigrateLock = "yanshi_migrate"

// lockMigrations 取得迁移锁。轮询 pg_try_advisory_lock，而不是用 pg_advisory_lock 阻塞等待：阻塞等待的语句本身是一个
// 打开的事务，而持锁一方执行的 CREATE INDEX CONCURRENTLY 要等所有更早的事务结束，二者互相等待（死锁）。
func lockMigrations(ctx context.Context, conn *pgxpool.Conn) error {
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, MigrateLock).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// NoTransaction 标记不能在事务中执行的迁移（如 CREATE INDEX CONCURRENTLY，大表上建索引不锁表）：写在迁移文件的
// 第一行。这样的迁移只能包含一条语句，并且须可以重复执行（如带 IF NOT EXISTS）：执行成功、记录版本之前进程退出时，
// 下次会再执行一遍。
const NoTransaction = "-- yanshi:no-transaction"

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	files, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []migration
	for _, f := range files {
		v, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(f, "migrations/"), "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad version prefix", f)
		}
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: f, sql: string(b)})
	}
	return out, nil
}

// Migrate 应用尚未应用的迁移（docs/design/m2-scale-test.md §6）。生产部署中由独立的 yanshi migrate 任务执行，
// serve 以 -migrate=false 启动、只检查（Pending）；开发时 serve 启动时自行迁移。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error { return MigrateFS(ctx, pool, migrations) }

// MigrateFS 按版本号顺序应用 fsys 中 migrations/*.sql 里尚未应用的迁移。多个实例并发执行时以迁移锁串行化
// （MigrateLock）。每个迁移在自己的事务中执行并记录版本，标记了 NoTransaction 的除外。
func MigrateFS(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	all, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if err := lockMigrations(ctx, conn); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, MigrateLock)
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY)`); err != nil {
		return err
	}
	for _, m := range all {
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if strings.HasPrefix(m.sql, NoTransaction) {
			if _, err := conn.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
				return err
			}
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Pending 返回尚未应用的迁移文件名。
func Pending(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	return PendingFS(ctx, pool, migrations)
}

func PendingFS(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	all, err := loadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	applied := map[int]bool{}
	if exists {
		rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return nil, err
		}
		vs, err := pgx.CollectRows(rows, pgx.RowTo[int32])
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			applied[int(v)] = true
		}
	}
	var out []string
	for _, m := range all {
		if !applied[m.version] {
			out = append(out, m.name)
		}
	}
	return out, nil
}

// IsUniqueViolation 判断错误是否为唯一约束冲突。
func IsUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
