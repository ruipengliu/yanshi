package app

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/pg"
	"yanshi/internal/shard"
)

// SinglePoolSelector 只装配库 0；C5 替换此闭包的选库部分，存储仍提交明确 Target。
// 非零分片有界失败，不能偷偷查询本地库或遍历其他库。
func SinglePoolSelector(pool *pgxpool.Pool, locator shard.Locator) pg.PoolSelector {
	return func(target shard.Target) (*pgxpool.Pool, error) {
		id, err := locator.Locate(target)
		if err != nil {
			return nil, err
		}
		if id != 0 {
			return nil, fmt.Errorf("app: shard %d is not assembled", id)
		}
		return pool, nil
	}
}
