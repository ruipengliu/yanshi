package shard

import (
	"fmt"
)

// Kind 区分同一字符串的不同访问目的，Session 数据与索引不能混用。
type Kind string

const (
	SessionData  Kind = "session_data"
	SessionIndex Kind = "session_index"
	EndUser      Kind = "end_user"
	Object       Kind = "object"
	Derived      Kind = "derived"
)

type Target struct {
	Kind Kind   `json:"kind"`
	Key  string `json:"key"`
}

// Locator 只选唯一目标，不持有连接，不证明对象归属，也不在未命中时尝试其他库。
// 零值无效；单库必须显式使用 DefaultLocator。
type Locator struct {
	table  Table
	compat Compatibility
}

func NewLocator(table Table, compat Compatibility) (Locator, error) {
	if err := ValidateActivation(table, compat); err != nil {
		return Locator{}, err
	}
	return Locator{table: table, compat: compat}, nil
}

func DefaultLocator() Locator { return defaultLocator }

var defaultLocator = singleLocator()

func singleLocator() Locator {
	t, _ := NewTable(Config{Shards: []int{0}, Ranges: []Range{{First: 0, Last: SlotCount - 1, Shard: 0}}})
	c, _ := NewCompatibility(t, CompatibilityConfig{Version: "single-v1", LegacyDataInShard0: true})
	l, _ := NewLocator(t, c)
	return l
}

func (l Locator) Locate(target Target) (int, error) {
	if !l.table.valid || !l.compat.valid || !validTarget(target) {
		return 0, fmt.Errorf("shard: invalid locator or target")
	}
	// 历史命名空间碰撞只能由停写预检确认后的显式声明覆盖；普通损坏 ID 仍拒绝。
	if entry, ok := l.compat.entries[target]; ok && (target.Kind == EndUser || entry.Historical) {
		return entry.Shard, nil
	}
	if target.Kind == EndUser {
		return l.table.Shard(Hash(target.Key), false)
	}
	var slot int
	var legacy bool
	var err error
	switch target.Kind {
	case SessionData:
		slot, legacy, err = SessionSlotOf(target.Key)
	case SessionIndex:
		slot, legacy, err = EndUserSlotOfSession(target.Key)
	case Object, Derived:
		slot, legacy, err = SlotOf(target.Key)
	}
	if err != nil {
		return 0, err
	}
	if !legacy {
		return l.table.Shard(slot, false)
	}
	if entry, ok := l.compat.entries[target]; ok {
		return entry.Shard, nil
	}
	if l.compat.legacyDataInShard0 {
		return 0, nil
	}
	return 0, fmt.Errorf("shard: legacy target has no compatibility entry")
}

func validTarget(t Target) bool {
	if t.Key == "" {
		return false
	}
	switch t.Kind {
	case SessionData, SessionIndex, EndUser, Object, Derived:
		return true
	}
	return false
}

// ValidateActivation 无历史键清单时无法证明哪些 EndUser 槽安全，只能要求全部留在库 0。
// 非空清单的完整性由停写迁移校验负责，不能从键字符串或条目数量推断。
func ValidateActivation(table Table, compat Compatibility) error {
	if !table.valid || !compat.valid {
		return fmt.Errorf("shard: invalid table or compatibility map")
	}
	for _, entry := range compat.entries {
		if !table.known[entry.Shard] {
			return fmt.Errorf("shard: compatibility shard is not registered")
		}
	}
	if compat.legacyDataInShard0 && len(compat.entries) == 0 {
		for _, id := range table.slots {
			if id != 0 {
				return fmt.Errorf("shard: unmapped legacy EndUsers require all slots in shard 0")
			}
		}
	}
	return nil
}

// Identity 供 C5 就绪检查比较同一份槽位表、兼容清单与回退声明。
func (l Locator) Identity() (version, digest string) { return l.compat.version, l.digest() }
