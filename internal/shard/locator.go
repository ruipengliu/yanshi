package shard

import (
	"fmt"
	"sort"
)

// Kind 区分同一字符串的不同访问目的，Session 数据与索引不能混用。
type Kind string

const (
	SessionData  Kind = "session_data"
	SessionIndex Kind = "session_index"
	EndUser      Kind = "end_user"
	Object       Kind = "object"
)

type Target struct {
	Kind Kind   `json:"kind"`
	Key  string `json:"key"`
}

// Locator 只选唯一目标，不持有连接，不证明对象归属，也不在未命中时尝试其他库。
// 零值无效；单库必须显式使用 DefaultLocator。
type Locator struct {
	table   Table
	version string
}

// NewLocator 的 version 是部署配置的版本标签，与槽位表一起构成 Identity，供各进程核对使用同一份配置。
func NewLocator(table Table, version string) (Locator, error) {
	if !table.valid || version == "" {
		return Locator{}, fmt.Errorf("shard: valid table and version are required")
	}
	return Locator{table: table, version: version}, nil
}

func DefaultLocator() Locator { return defaultLocator }

var defaultLocator = singleLocator()

func singleLocator() Locator {
	t, _ := NewTable(Config{Shards: []int{0}, Ranges: []Range{{First: 0, Last: SlotCount - 1, Shard: 0}}})
	l, _ := NewLocator(t, "single-v1")
	return l
}

// Locate 只认带版本的 ID（ADR-0051）：EndUser 按哈希取槽，其余从 ID 中读出槽位；无版本或损坏的 ID 报错，不回退。
func (l Locator) Locate(target Target) (int, error) {
	if !l.table.valid || !validTarget(target) {
		return 0, fmt.Errorf("shard: invalid locator or target")
	}
	var slot int
	var err error
	switch target.Kind {
	case EndUser:
		slot = Hash(target.Key)
	case SessionData:
		slot, err = SessionSlotOf(target.Key)
	case SessionIndex:
		slot, err = EndUserSlotOfSession(target.Key)
	case Object:
		slot, err = SlotOf(target.Key)
	}
	if err != nil {
		return 0, err
	}
	return l.table.Shard(slot)
}

func validTarget(t Target) bool {
	if t.Key == "" {
		return false
	}
	switch t.Kind {
	case SessionData, SessionIndex, EndUser, Object:
		return true
	}
	return false
}

// Identity 供 C5 就绪检查比较各进程使用的是同一份槽位表。
func (l Locator) Identity() (version, digest string) { return l.version, l.digest() }

// Shards 返回登记分片的有序副本；稳定编号不是数组下标。
func (l Locator) Shards() []int {
	var out []int
	for id := range l.table.known {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// Slots 公开固定槽位的归属，不包含用户 ID。
func (l Locator) Slots(id int) []int {
	var slots []int
	for slot, owner := range l.table.slots {
		if owner == id {
			slots = append(slots, slot)
		}
	}
	return slots
}
