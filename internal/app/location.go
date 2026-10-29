package app

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/pg"
	"yanshi/internal/shard"
)

// SinglePoolSelector 只装配库 0，保留单库与定位测试入口。
// 非零分片有界失败，不能偷偷查询本地库或遍历其他库。
func SinglePoolSelector(pool *pgxpool.Pool, locator shard.Locator) pg.PoolSelector {
	return ShardPoolSelector(pool, locator, 0)
}

// ShardPoolSelector 不选第二个库：防止裸 ID 路径绕过装配层或误入其他分片。
func ShardPoolSelector(pool *pgxpool.Pool, locator shard.Locator, own int) pg.PoolSelector {
	return func(target shard.Target) (*pgxpool.Pool, error) {
		id, err := locator.Locate(target)
		if err != nil {
			return nil, err
		}
		if id != own {
			return nil, fmt.Errorf("app: shard %d is not assembled", id)
		}
		return pool, nil
	}
}
