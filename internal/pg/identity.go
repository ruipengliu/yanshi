package pg

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseID 在主备切换后仍表示同一个物理数据库，避免把 schema 或同名异库当成分片。
// 多库部署的连接角色须有 pg_control_system() 的 EXECUTE 权限（或 pg_monitor）。
type DatabaseID struct {
	System string
	OID    uint32
}

func DatabaseIdentity(ctx context.Context, pool *pgxpool.Pool) (DatabaseID, string, error) {
	var id DatabaseID
	var schema string
	err := pool.QueryRow(ctx, `SELECT c.system_identifier::text, d.oid, current_schema()
		FROM pg_control_system() c CROSS JOIN pg_database d WHERE d.datname = current_database()`).Scan(&id.System, &id.OID, &schema)
	return id, schema, err
}
