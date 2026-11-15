package shard

import (
	"errors"
	"fmt"
	"strings"
)

const slotAlphabet = "0123456789abcdefghijklmnopqrstuv"

// ErrInvalidID 表示 ID 无法解析（无版本、损坏或版本未知）。这样的 ID 不可能指向任何对象：对外接口把它当作不存在
// （404 / not_found），不能当作内部错误，否则客户端随手拼的 ID 会得到 500。
var ErrInvalidID = errors.New("ids: invalid ID")

// SlotOf 只接受带版本的 ID：没有版本标记、损坏或版本未知的 ID 一律报错。系统上线前删除了旧格式 ID 的兼容
// （ADR-0051），不再有需要回退到库 0 的无版本 ID。
// 槽位仅供定位，不证明归属；读到对象后仍须核对真实 Session / EndUser 与业务线。
func SlotOf(id string) (int, error) {
	p, err := parse(id)
	return p.slot, err
}

func SessionSlotOf(id string) (int, error) {
	p, err := parseSession(id)
	return p.slot, err
}

// EndUserSlotOfSession 定位 ByEndUser 的 Session 索引，不用于 Session 日志。
func EndUserSlotOfSession(id string) (int, error) {
	p, err := parseSession(id)
	return p.endUserSlot, err
}

type parsedID struct {
	prefix      string
	slot        int
	endUserSlot int
}

func parseSession(id string) (parsedID, error) {
	p, err := parse(id)
	if err == nil && p.prefix != "ses" {
		err = fmt.Errorf("%w: expected a Session ID", ErrInvalidID)
	}
	return p, err
}

func parse(id string) (parsedID, error) {
	if id == "" {
		return parsedID{}, fmt.Errorf("%w: empty ID", ErrInvalidID)
	}
	parts := strings.SplitN(id, "_", 3)
	if len(parts) < 2 || !versionMarker(parts[1]) {
		return parsedID{}, fmt.Errorf("%w: unversioned ID", ErrInvalidID)
	}
	if parts[1] != "v1" || len(parts) != 3 || !validPrefix(parts[0]) {
		return parsedID{}, fmt.Errorf("%w: invalid or unsupported ID version", ErrInvalidID)
	}
	p := parsedID{prefix: parts[0]}
	slot, rest, ok := strings.Cut(parts[2], "_")
	var err error
	if p.slot, err = decodeSlot(slot); !ok || err != nil {
		return parsedID{}, fmt.Errorf("%w: invalid slot", ErrInvalidID)
	}
	if p.prefix == "ses" {
		slot, rest, ok = strings.Cut(rest, "_")
		if p.endUserSlot, err = decodeSlot(slot); !ok || err != nil {
			return parsedID{}, fmt.Errorf("%w: invalid EndUser slot", ErrInvalidID)
		}
	}
	if !validToken(rest) {
		return parsedID{}, fmt.Errorf("%w: invalid token", ErrInvalidID)
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
		return 0, fmt.Errorf("%w: slot must have three digits", ErrInvalidID)
	}
	slot := 0
	for _, c := range s {
		digit := strings.IndexRune(slotAlphabet, c)
		if digit < 0 {
			return 0, fmt.Errorf("%w: invalid slot digit", ErrInvalidID)
		}
		slot = slot*32 + digit
	}
	if slot >= SlotCount {
		return 0, fmt.Errorf("%w: slot out of range", ErrInvalidID)
	}
	return slot, nil
}
