package app

import (
	"fmt"

	"yanshi/internal/shard"
)

type ShardDatabase struct {
	ID        int    `json:"id"`
	SQLDSN    string `json:"sql_dsn"`
	DirectDSN string `json:"direct_dsn"`
}

// ShardConfig 是所有角色共用的部署制品；DSN 不参与公开路由摘要，也不写日志。
type ShardConfig struct {
	Table         shard.Config              `json:"table"`
	Compatibility shard.CompatibilityConfig `json:"compatibility"`
	Databases     []ShardDatabase           `json:"databases"`
}

func ParseShardConfig(data []byte) (ShardConfig, shard.Locator, error) {
	var c ShardConfig
	if err := shard.DecodeConfig(data, &c); err != nil {
		return c, shard.Locator{}, err
	}
	t, err := shard.NewTable(c.Table)
	if err != nil {
		return c, shard.Locator{}, err
	}
	compat, err := shard.NewCompatibility(t, c.Compatibility)
	if err != nil {
		return c, shard.Locator{}, err
	}
	if err := shard.ValidateActivation(t, compat); err != nil {
		return c, shard.Locator{}, err
	}
	l, err := shard.NewLocator(t, compat)
	if err != nil {
		return c, l, err
	}
	allZero := true
	for slot := range shard.SlotCount {
		id, _ := t.Shard(slot, false)
		allZero = allZero && id == 0
	}
	// 静态兼容清单不能覆盖迁出 legacy Session 未来生成的旧 ID；P4 不提供旧数据迁出。
	if !allZero && (c.Compatibility.LegacyDataInShard0 || len(c.Compatibility.Entries) > 0) {
		return c, l, fmt.Errorf("app: P4 multiple shards require a new cell without legacy data or compatibility entries")
	}
	for _, e := range c.Compatibility.Entries {
		if e.Shard != 0 {
			return c, l, fmt.Errorf("app: P4 cannot move legacy data out of shard 0")
		}
	}
	known := map[int]bool{}
	for _, id := range l.Shards() {
		known[id] = true
	}
	seen := map[int]bool{}
	sqlDSNs, directDSNs := map[string]bool{}, map[string]bool{}
	for _, db := range c.Databases {
		if !known[db.ID] || seen[db.ID] || db.SQLDSN == "" || db.DirectDSN == "" {
			return c, l, fmt.Errorf("app: each registered shard requires exactly one SQL and direct DSN")
		}
		if sqlDSNs[db.SQLDSN] || directDSNs[db.DirectDSN] {
			return c, l, fmt.Errorf("app: database DSNs must be distinct across shards")
		}
		seen[db.ID] = true
		sqlDSNs[db.SQLDSN], directDSNs[db.DirectDSN] = true, true
	}
	if len(seen) != len(known) {
		return c, l, fmt.Errorf("app: missing shard database")
	}
	return c, l, nil
}

// 缺失 id 不能被整数零值当成显式登记的控制库。
func (d *ShardDatabase) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        *int   `json:"id"`
		SQLDSN    string `json:"sql_dsn"`
		DirectDSN string `json:"direct_dsn"`
	}
	if err := shard.DecodeConfig(data, &raw); err != nil {
		return err
	}
	if raw.ID == nil {
		return fmt.Errorf("app: shard database requires id")
	}
	*d = ShardDatabase{ID: *raw.ID, SQLDSN: raw.SQLDSN, DirectDSN: raw.DirectDSN}
	return nil
}
