package ids

import (
	"fmt"
	"strings"

	"yanshi/internal/shard"
)

const slotAlphabet = "0123456789abcdefghijklmnopqrstuv"

// Session 每次只消耗一个 ID，保留模拟测试中生成器的调用轨迹。
func (g Generator) Session(endUser string) (string, error) {
	return SessionID(g(), endUser)
}

// SessionID 也供确定性派生的子 Session 使用；token 沿用原有派生算法。
// 自身槽位由未加槽位的 ses_<token> 决定，避免对含自身槽位的 ID 求哈希。
func SessionID(token, endUser string) (string, error) {
	if !validToken(token) || endUser == "" {
		return "", fmt.Errorf("ids: session needs a token and EndUser")
	}
	return "ses_v1_" + encodeSlot(shard.Hash("ses_"+token)) + "_" + encodeSlot(shard.Hash(endUser)) + "_" + token, nil
}

func (g Generator) ForSession(prefix, sessionID string) (string, error) {
	return WithSession(prefix, g(), sessionID)
}

// WithSession 只继承所属 Session 的槽位，工件或调用自己的 token 不参与选槽。
// 旧 Session 的新对象也保持旧格式，不能因槽位 0 被迁走而离开兼容库。
func WithSession(prefix, token, sessionID string) (string, error) {
	slot, legacy, err := SessionSlotOf(sessionID)
	if err != nil {
		return "", err
	}
	if !validPrefix(prefix) || prefix == "ses" || !validToken(token) {
		return "", fmt.Errorf("ids: invalid object prefix or token")
	}
	if legacy {
		return prefix + "_" + token, nil
	}
	return prefix + "_v1_" + encodeSlot(slot) + "_" + token, nil
}

func (g Generator) ForEndUser(prefix, endUser string) (string, error) {
	return WithEndUser(prefix, g(), endUser)
}

// WithEndUser 不接受业务线，跨业务线的 Memory 与 Grant 必须仍在同一 EndUser 的库。
// 调用方须已完成旧 EndUser 数据的兼容安排；外部 EndUser ID 自身没有版本可供判断。
func WithEndUser(prefix, token, endUser string) (string, error) {
	if !validPrefix(prefix) || prefix == "ses" || !validToken(token) || endUser == "" {
		return "", fmt.Errorf("ids: invalid object prefix, token or EndUser")
	}
	return prefix + "_v1_" + encodeSlot(shard.Hash(endUser)) + "_" + token, nil
}

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

func encodeSlot(slot int) string {
	return string([]byte{slotAlphabet[slot>>10], slotAlphabet[(slot>>5)&31], slotAlphabet[slot&31]})
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
	if slot >= shard.SlotCount {
		return 0, fmt.Errorf("ids: slot out of range")
	}
	return slot, nil
}
