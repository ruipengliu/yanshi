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

// ParseDeployment 把版本标签与槽位表作为一个原子部署版本载入；不打开数据库。
func ParseDeployment(data []byte) (Locator, error) {
	var c struct {
		Version string `json:"version"`
		Table   Config `json:"table"`
	}
	if err := strictJSON(data, &c); err != nil {
		return Locator{}, err
	}
	t, err := NewTable(c.Table)
	if err != nil {
		return Locator{}, err
	}
	return NewLocator(t, c.Version)
}

func (l Locator) digest() string {
	shards := make([]int, 0, len(l.table.known))
	for id := range l.table.known {
		shards = append(shards, id)
	}
	sort.Ints(shards)
	b, _ := json.Marshal(struct {
		Version string
		Slots   [SlotCount]int
		Shards  []int
	}{l.version, l.table.slots, shards})
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
