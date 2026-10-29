package pg

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/shard"
)

// PoolSelector 接受访问目的而非猜测字符串；装配层选库后，分片内只断言本库归属。
type PoolSelector func(shard.Target) (*pgxpool.Pool, error)

// SelectPool 保留直接构造单库存储的测试入口，但也必须经过定位器。
func SelectPool(selectPool PoolSelector, pool *pgxpool.Pool, target shard.Target) (*pgxpool.Pool, error) {
	if selectPool != nil {
		return selectPool(target)
	}
	id, err := shard.DefaultLocator().Locate(target)
	if err != nil {
		return nil, err
	}
	if id != 0 {
		return nil, fmt.Errorf("pg: shard is not available")
	}
	return pool, nil
}
