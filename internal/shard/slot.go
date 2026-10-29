// Package shard 固定键到槽位、槽位到分片的规则；不持有数据库连接（ADR-0051）。
package shard

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// SlotCount 不随库数变化，否则扩容会改变已有键的归属。
const SlotCount = 1024

// Hash 按原始 UTF-8 字节取 SHA-256 的前八字节（大端），再对固定槽位数取模。
// EndUser 不拼业务线；带槽位的 Session 必须读 ID 的槽位，不能重新哈希整个 ID。
func Hash(key string) int {
	sum := sha256.Sum256([]byte(key))
	return int(binary.BigEndian.Uint64(sum[:8]) % SlotCount)
}

// Range 的闭区间便于审查部署配置；NewTable 拒绝重叠和遗漏，避免静默落入库 0。
type Range struct {
	First int `json:"first"`
	Last  int `json:"last"`
	Shard int `json:"shard"`
}

type Config struct {
	Shards []int   `json:"shards"`
	Ranges []Range `json:"ranges"`
}

// Table 在构造时复制配置，调用方之后修改切片不会改变路由。
type Table struct {
	slots [SlotCount]int
	valid bool
}

func NewTable(c Config) (Table, error) {
	var table Table
	known := make(map[int]bool, len(c.Shards))
	for _, id := range c.Shards {
		if id < 0 || known[id] {
			return Table{}, fmt.Errorf("shard: invalid or duplicate shard %d", id)
		}
		known[id] = true
	}
	if !known[0] {
		return Table{}, fmt.Errorf("shard: compatibility shard 0 is required")
	}
	var assigned [SlotCount]bool
	for _, r := range c.Ranges {
		if r.First < 0 || r.Last >= SlotCount || r.First > r.Last || !known[r.Shard] {
			return Table{}, fmt.Errorf("shard: invalid range %+v", r)
		}
		for slot := r.First; slot <= r.Last; slot++ {
			if assigned[slot] {
				return Table{}, fmt.Errorf("shard: duplicate slot %d", slot)
			}
			table.slots[slot], assigned[slot] = r.Shard, true
		}
	}
	for slot, ok := range assigned {
		if !ok {
			return Table{}, fmt.Errorf("shard: missing slot %d", slot)
		}
	}
	table.valid = true
	return table, nil
}

// ParseTable 不接受未知字段或尾随 JSON，配置拼写错误不能变成默认路由。
func ParseTable(data []byte) (Table, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Table{}, fmt.Errorf("shard: config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Table{}, fmt.Errorf("shard: trailing config data")
	}
	return NewTable(c)
}

// Shard 的 legacy 分支独立于槽位 0 的配置：旧数据在兼容库 0，不是槽位 0。
// 搬走旧数据的部署须先在外层接入显式兼容映射，不能继续走此默认分支。
func (t Table) Shard(slot int, legacy bool) (int, error) {
	if !t.valid || slot < 0 || slot >= SlotCount {
		return 0, fmt.Errorf("shard: invalid table or slot %d", slot)
	}
	if legacy {
		return 0, nil
	}
	return t.slots[slot], nil
}
