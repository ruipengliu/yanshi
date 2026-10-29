package shard

import (
	"fmt"
	"strings"
)

const slotAlphabet = "0123456789abcdefghijklmnopqrstuv"

// SlotOf 的 legacy 只表示没有版本标记；带标记但损坏或版本未知的 ID 必须报错，不能回退库 0。
// 槽位仅供定位，不证明归属；读到对象后仍须核对真实 Session / EndUser 与业务线。
func SlotOf(id string) (slot int, legacy bool, err error) {
	p, err := parse(id)
	return p.slot, p.legacy, err
}

func SessionSlotOf(id string) (slot int, legacy bool, err error) {
	p, err := parseSession(id)
	return p.slot, p.legacy, err
}

// EndUserSlotOfSession 定位 ByEndUser 的 Session 索引，不用于 Session 日志。
func EndUserSlotOfSession(id string) (slot int, legacy bool, err error) {
	p, err := parseSession(id)
	return p.endUserSlot, p.legacy, err
}

type parsedID struct {
	prefix      string
	slot        int
	endUserSlot int
	legacy      bool
}

func parseSession(id string) (parsedID, error) {
	p, err := parse(id)
	if err == nil && !p.legacy && p.prefix != "ses" {
		err = fmt.Errorf("ids: expected a Session ID")
	}
	return p, err
}

func parse(id string) (parsedID, error) {
	if id == "" {
		return parsedID{}, fmt.Errorf("ids: empty ID")
	}
	parts := strings.SplitN(id, "_", 3)
	if len(parts) < 2 || !versionMarker(parts[1]) {
		return parsedID{legacy: true}, nil
	}
	if parts[1] != "v1" || len(parts) != 3 || !validPrefix(parts[0]) {
		return parsedID{}, fmt.Errorf("ids: invalid or unsupported ID version")
	}
	p := parsedID{prefix: parts[0]}
	slot, rest, ok := strings.Cut(parts[2], "_")
	var err error
	if p.slot, err = decodeSlot(slot); !ok || err != nil {
		return parsedID{}, fmt.Errorf("ids: invalid slot")
	}
	if p.prefix == "ses" {
		slot, rest, ok = strings.Cut(rest, "_")
		if p.endUserSlot, err = decodeSlot(slot); !ok || err != nil {
			return parsedID{}, fmt.Errorf("ids: invalid EndUser slot")
		}
	}
	if !validToken(rest) {
		return parsedID{}, fmt.Errorf("ids: invalid token")
	}
	return p, nil
}

func versionMarker(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func decodeSlot(s string) (int, error) {
	if len(s) != 3 {
		return 0, fmt.Errorf("ids: slot must have three digits")
	}
	slot := 0
	for _, c := range s {
		digit := strings.IndexRune(slotAlphabet, c)
		if digit < 0 {
			return 0, fmt.Errorf("ids: invalid slot digit")
		}
		slot = slot*32 + digit
	}
	if slot >= SlotCount {
		return 0, fmt.Errorf("ids: slot out of range")
	}
	return slot, nil
}

// VersionedNamespace 只识别保留命名空间，不证明 ID 有效或属于新数据。
func VersionedNamespace(id string) bool {
	parts := strings.SplitN(id, "_", 3)
	return len(parts) >= 2 && versionMarker(parts[1])
}
