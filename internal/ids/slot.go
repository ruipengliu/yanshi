package ids

import (
	"fmt"

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
func WithSession(prefix, token, sessionID string) (string, error) {
	slot, err := SessionSlotOf(sessionID)
	if err != nil {
		return "", err
	}
	if !validPrefix(prefix) || prefix == "ses" || !validToken(token) {
		return "", fmt.Errorf("ids: invalid object prefix or token")
	}
	return prefix + "_v1_" + encodeSlot(slot) + "_" + token, nil
}

func (g Generator) ForEndUser(prefix, endUser string) (string, error) {
	return WithEndUser(prefix, g(), endUser)
}

// WithEndUser 不接受业务线，跨业务线的 Memory 与 Grant 必须仍在同一 EndUser 的库。
func WithEndUser(prefix, token, endUser string) (string, error) {
	if !validPrefix(prefix) || prefix == "ses" || !validToken(token) || endUser == "" {
		return "", fmt.Errorf("ids: invalid object prefix, token or EndUser")
	}
	return prefix + "_v1_" + encodeSlot(shard.Hash(endUser)) + "_" + token, nil
}

// SlotOf 只接受带版本的 ID，无版本、损坏或版本未知都报错（ADR-0051）。
// 槽位仅供定位，不证明归属；读到对象后仍须核对真实 Session / EndUser 与业务线。
func SlotOf(id string) (int, error) {
	return shard.SlotOf(id)
}

func SessionSlotOf(id string) (int, error) {
	return shard.SessionSlotOf(id)
}

// EndUserSlotOfSession 定位 ByEndUser 的 Session 索引，不用于 Session 日志。
func EndUserSlotOfSession(id string) (int, error) {
	return shard.EndUserSlotOfSession(id)
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
