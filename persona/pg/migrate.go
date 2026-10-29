package pg

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// NoTransaction 供后续大表的并发索引迁移使用；文件须只含一条可重复执行的语句。
const NoTransaction = "-- yanshi:no-transaction"

// Migrate 需要直连 PostgreSQL，不能经不保留会话状态的事务池。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return MigrateFS(ctx, pool, migrations)
}

// MigrateFS 允许测试故障与非事务迁移；版本只新增，不修改已应用的文件。
func MigrateFS(ctx context.Context, pool *pgxpool.Pool, files fs.FS) error {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// 等锁时不保持开放事务，避免阻塞持锁方的 CREATE INDEX CONCURRENTLY。
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('persona_migrate'))`).Scan(&locked); err != nil {
			return err
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, `SELECT pg_advisory_unlock(hashtext('persona_migrate'))`); err != nil {
			// 无法确认解锁时不能把带会话锁的连接交回池。
			_ = conn.Conn().Close(cleanup)
		}
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS persona_schema_migrations (version integer PRIMARY KEY)`); err != nil {
		return err
	}
	previous := 0
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil || version <= previous {
			return fmt.Errorf("invalid persona migration version: %s", name)
		}
		previous = version
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM persona_schema_migrations WHERE version=$1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if strings.HasPrefix(string(data), NoTransaction+"\n") {
			if _, err := conn.Exec(ctx, string(data)); err != nil {
				return fmt.Errorf("persona migration %s: %w", name, err)
			}
			if _, err := conn.Exec(ctx, `INSERT INTO persona_schema_migrations(version) VALUES ($1)`, version); err != nil {
				return err
			}
			continue
		}
		if err := migrateOne(ctx, conn, version, string(data)); err != nil {
			return fmt.Errorf("persona migration %s: %w", name, err)
		}
	}
	return nil
}

func migrateOne(ctx context.Context, conn *pgxpool.Conn, version int, sql string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer Rollback(tx)
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO persona_schema_migrations(version) VALUES ($1)`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
