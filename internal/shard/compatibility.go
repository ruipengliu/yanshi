package shard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// CompatibilityEntry 是停写迁移的权威产物；不提供在线增删接口。
// Historical 只用于已核实的保留命名空间碰撞，不能用于宽松解析新 ID。
type CompatibilityEntry struct {
	Target
	Shard      int  `json:"shard"`
	Historical bool `json:"historical,omitempty"`
}

// UnmarshalJSON 要求目标分片显式出现，缺字段不能被 JSON 的整数零值误当成权威库 0。
func (e *CompatibilityEntry) UnmarshalJSON(data []byte) error {
	var raw struct {
		Kind       Kind   `json:"kind"`
		Key        string `json:"key"`
		Shard      *int   `json:"shard"`
		Historical bool   `json:"historical,omitempty"`
	}
	if err := strictJSON(data, &raw); err != nil {
		return err
	}
	if raw.Shard == nil {
		return fmt.Errorf("shard: compatibility entry requires an explicit shard")
	}
	*e = CompatibilityEntry{Target: Target{Kind: raw.Kind, Key: raw.Key}, Shard: *raw.Shard, Historical: raw.Historical}
	return nil
}

type CompatibilityConfig struct {
	Version            string               `json:"version"`
	LegacyDataInShard0 bool                 `json:"legacy_data_in_shard0"`
	Entries            []CompatibilityEntry `json:"entries"`
}

type Compatibility struct {
	version            string
	legacyDataInShard0 bool
	entries            map[Target]CompatibilityEntry
	valid              bool
}

func NewCompatibility(table Table, c CompatibilityConfig) (Compatibility, error) {
	if !table.valid || c.Version == "" {
		return Compatibility{}, fmt.Errorf("shard: table and compatibility version are required")
	}
	out := Compatibility{version: c.Version, legacyDataInShard0: c.LegacyDataInShard0, entries: make(map[Target]CompatibilityEntry), valid: true}
	for _, e := range c.Entries {
		if !validTarget(e.Target) || !table.known[e.Shard] {
			return Compatibility{}, fmt.Errorf("shard: invalid compatibility entry")
		}
		if _, ok := out.entries[e.Target]; ok {
			return Compatibility{}, fmt.Errorf("shard: duplicate compatibility target")
		}
		if e.Kind != EndUser && !e.Historical {
			_, legacy, err := SlotOf(e.Key)
			if err != nil || !legacy {
				return Compatibility{}, fmt.Errorf("shard: versioned historical entry requires historical=true")
			}
		}
		out.entries[e.Target] = e
	}
	return out, nil
}

func ParseCompatibility(table Table, data []byte) (Compatibility, error) {
	var c CompatibilityConfig
	if err := strictJSON(data, &c); err != nil {
		return Compatibility{}, err
	}
	return NewCompatibility(table, c)
}

// ParseDeployment 把槽位表与兼容清单作为一个原子部署版本载入；不打开数据库。
func ParseDeployment(data []byte) (Locator, error) {
	var c struct {
		Table         Config              `json:"table"`
		Compatibility CompatibilityConfig `json:"compatibility"`
	}
	if err := strictJSON(data, &c); err != nil {
		return Locator{}, err
	}
	t, err := NewTable(c.Table)
	if err != nil {
		return Locator{}, err
	}
	compat, err := NewCompatibility(t, c.Compatibility)
	if err != nil {
		return Locator{}, err
	}
	return NewLocator(t, compat)
}

func (l Locator) digest() string {
	entries := make([]CompatibilityEntry, 0, len(l.compat.entries))
	for _, e := range l.compat.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].Key < entries[j].Key
	})
	shards := make([]int, 0, len(l.table.known))
	for id := range l.table.known {
		shards = append(shards, id)
	}
	sort.Ints(shards)
	b, _ := json.Marshal(struct {
		Slots         [SlotCount]int
		Shards        []int
		Compatibility CompatibilityConfig
	}{l.table.slots, shards, CompatibilityConfig{Version: l.compat.version, LegacyDataInShard0: l.compat.legacyDataInShard0, Entries: entries}})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func strictJSON(data []byte, dest any) error {
	if err := rejectDuplicateFields(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("shard: trailing config data")
	}
	return nil
}

// encoding/json 默认接受重复字段并取最后一个；部署配置不能容许这种歧义。
func rejectDuplicateFields(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return fmt.Errorf("shard: duplicate JSON field")
				}
				seen[s] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("shard: unexpected JSON delimiter")
		}
		_, err = dec.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("shard: trailing config data")
	}
	return nil
}

// DecodeConfig 让装配配置复用严格 JSON 校验，新增连接字段仍拒绝重复字段与尾随数据。
func DecodeConfig(data []byte, dest any) error { return strictJSON(data, dest) }
