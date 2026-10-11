// Package delegation 实现 Agent 之间的委托（docs/design/m5-multi-agent.md）：Agent 在模型眼里是一种 Capability，
// 在平台眼里是一次路由调用，目标是由调用派生的子 Session。子 Run 继承 Run 的全部语义（租约、fencing、挂起、
// 中断、删除、计量），终态作为父调用的外部结果写回。
package delegation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"yanshi/internal/session"
)

// Prefix 是委托目标的保留 Node ID 前缀：完整 ID 为 "@agent/<子 Session ID>"（node.Reserved 拒绝设备以它接入）。
const Prefix = "@agent/"

// NodeID 返回子 Session 对应的路由目标。
func NodeID(childSession string) string { return Prefix + childSession }

// IsNode 报告 id 是否是委托目标。
func IsNode(id string) bool { return strings.HasPrefix(id, Prefix) }

// ChildSession 返回委托目标对应的子 Session ID。
func ChildSession(nodeID string) string { return strings.TrimPrefix(nodeID, Prefix) }

// ChildSessionID 由父 Session 与调用 ID 推导子 Session ID：重复派发（重新投递、Worker 崩溃后接管）命中同一个
// 子 Session，派发因此幂等。
func ChildSessionID(parentSession, callID string) string {
	h := sha256.Sum256([]byte(parentSession + "\x00" + callID))
	return "ses_" + hex.EncodeToString(h[:16])
}

// InputID 是子 Session 中委托输入的输入 ID：重复提交返回首次的结果（service.SubmitDelegated 去重），投影据此记下
// 委托 Run（session.State.Delegated）。
func InputID(callID string) string { return session.DelegatedInputID(callID) }

// Args 是委托工具的输入。
type Args struct {
	Task    string `json:"task"`
	Context string `json:"context"`
}

// ParseArgs 解析并检查委托工具的参数。
func ParseArgs(argumentsJSON string) (Args, error) {
	var a Args
	if err := json.Unmarshal([]byte(argumentsJSON), &a); err != nil {
		return a, fmt.Errorf("invalid arguments: %v", err)
	}
	if strings.TrimSpace(a.Task) == "" {
		return a, fmt.Errorf("invalid arguments: task is required")
	}
	return a, nil
}

// Input 是交给子 Run 的输入文字。
func (a Args) Input() string {
	if strings.TrimSpace(a.Context) == "" {
		return a.Task
	}
	return a.Task + "\n\n背景：\n" + a.Context
}
