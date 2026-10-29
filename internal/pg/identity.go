package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseID 随数据复制保持不变；只允许迁移入口初始化，serve 永远只读核对。
type DatabaseID struct {
	Instance string
	Shard    int
}

func DatabaseIdentity(ctx context.Context, pool *pgxpool.Pool) (DatabaseID, string, error) {
	var id DatabaseID
	var schema string
	err := pool.QueryRow(ctx, `SELECT instance_id::text, shard_id, current_schema()
		FROM public.shard_identity WHERE singleton`).Scan(&id.Instance, &id.Shard, &schema)
	return id, schema, err
}

// InitShardIdentity 不覆盖已有身份，重复迁移或两个 DSN 指向同一库都不能重新绑定分片号。
func InitShardIdentity(ctx context.Context, pool *pgxpool.Pool, shardID int) error {
	if shardID < 0 {
		return fmt.Errorf("pg: invalid shard identity")
	}
	_, err := pool.Exec(ctx, `INSERT INTO public.shard_identity(singleton,instance_id,shard_id)
		VALUES(true,gen_random_uuid(),$1) ON CONFLICT(singleton) DO NOTHING`, shardID)
	if err != nil {
		return err
	}
	id, _, err := DatabaseIdentity(ctx, pool)
	if err != nil {
		return err
	}
	if id.Shard != shardID {
		return fmt.Errorf("pg: shard identity mismatch")
	}
	return nil
}
