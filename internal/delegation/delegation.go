// Package delegation 实现 Agent 之间的委托（docs/design/m5-multi-agent.md）：Agent 在模型眼里是一种 Capability，
// 在平台眼里是一次路由调用，目标是由调用派生的子 Session。子 Run 继承 Run 的全部语义（租约、fencing、挂起、
// 中断、删除、计量），终态作为父调用的外部结果写回。
package delegation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"yanshi/internal/artifact"
	"yanshi/internal/ids"
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
// 子 Session，派发因此幂等。子 Session 自身独立选槽，不继承父 Session 的数据槽（ADR-0051 §3）。
func ChildSessionID(parentSession, callID, endUser string) (string, error) {
	if _, err := ids.SessionSlotOf(parentSession); err != nil {
		return "", err
	}
	if endUser == "" {
		return "", fmt.Errorf("delegation: EndUser is required")
	}
	h := sha256.Sum256([]byte(parentSession + "\x00" + callID))
	return ids.SessionID(hex.EncodeToString(h[:16]), endUser)
}

// InputID 是子 Session 中委托输入的输入 ID：重复提交返回首次的结果（service.SubmitDelegated 去重），投影据此记下
// 委托 Run（session.State.Delegated）。
func InputID(callID string) string { return session.DelegatedInputID(callID) }

// Args 是委托工具的输入。
type Args struct {
	Task    string `json:"task"`
	Context string `json:"context"`
	// Artifacts 是交给子 Run 的父 Session 工件（artifact://<id> 或工件 ID）：平台在派发时复制进子 Session，
	// 子 Run 只看得到点名的工件（ADR-0055）。
	Artifacts []string `json:"artifacts"`
}

// MaxArtifacts 是一次委托在每个方向上至多复制的工件数（ADR-0055）。
const MaxArtifacts = 8

// ParseArgs 解析并检查委托工具的参数；Artifacts 规整为去重的工件 ID。
func ParseArgs(argumentsJSON string) (Args, error) {
	var a Args
	if err := json.Unmarshal([]byte(argumentsJSON), &a); err != nil {
		return a, fmt.Errorf("invalid arguments: %v", err)
	}
	if strings.TrimSpace(a.Task) == "" {
		return a, fmt.Errorf("invalid arguments: task is required")
	}
	var ids []string
	for _, ref := range a.Artifacts {
		ref = strings.TrimSpace(ref)
		id, ok := artifact.ParseURI(ref)
		if !ok {
			id = ref
		}
		if !artifactID.MatchString(id) {
			return a, fmt.Errorf("invalid arguments: artifacts must be artifact://<id> references, got %q", ref)
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > MaxArtifacts {
		return a, fmt.Errorf("invalid arguments: at most %d artifacts per delegation, got %d", MaxArtifacts, len(ids))
	}
	a.Artifacts = ids
	return a, nil
}

// artifactID 匹配带版本的工件 ID（ADR-0051）。回复中的工件引用按它识别：模型看到的是
// "[artifact art_…: 名称, 类型, 大小]"，常常只写出 ID 而不带 artifact:// 前缀。
var artifactID = regexp.MustCompile(`^art_v1_[0-9a-v]{3}_[A-Za-z0-9_-]{1,64}$`)

var artifactRef = regexp.MustCompile(`art_v1_[0-9a-v]{3}_[A-Za-z0-9_-]{1,64}`)

// CopyID 推导复制进 dstSession 的工件 ID：同一次委托、同一个来源总得到同一个 ID，重复派发与重复回写因此
// 不产生第二份副本。
func CopyID(dstSession, callID, srcID string) (string, error) {
	h := sha256.Sum256([]byte(dstSession + "\x00" + callID + "\x00" + srcID))
	return ids.WithSession("art", hex.EncodeToString(h[:16]), dstSession)
}

// Input 是交给子 Run 的输入文字。
func (a Args) Input() string {
	if strings.TrimSpace(a.Context) == "" {
		return a.Task
	}
	return a.Task + "\n\n背景：\n" + a.Context
}
