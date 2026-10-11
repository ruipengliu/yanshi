// Package node 管理 Device 上的 Node：目录、待投递 Invocation（Inbox）与网关核心逻辑（Hub）。
// 语义见 docs/design/m1-device-nodes.md。
package node

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/auth"
)

var ErrNotFound = errors.New("node: not found")

// Scope 是 Node 可见性边界：只有同一 BusinessLine、同一 EndUser 的 Session 能使用该 Node（ADR-0002）。
type Scope struct {
	BusinessLine string
	EndUser      string
}

type Info struct {
	NodeID       string
	Scope        Scope
	Label        string
	Kind         string
	HostApp      string
	Capabilities []*v1.CapabilitySpec
	Online       bool
	LastSeen     time.Time
}

func (i *Info) Capability(name string) *v1.CapabilitySpec {
	for _, c := range i.Capabilities {
		if c.GetName() == name {
			return c
		}
	}
	return nil
}

type Directory interface {
	// Register 记录 Node 及其能力声明并标记在线，返回最终标签与连接代数。
	// 同一 NodeID 不能改换 Scope。
	Register(ctx context.Context, info Info) (label string, gen uint64, err error)
	// SetOffline 仅当 gen 仍是该 Node 最新连接代数时标记离线，避免旧连接的断开覆盖新连接。
	SetOffline(ctx context.Context, nodeID string, gen uint64) error
	Get(ctx context.Context, nodeID string) (*Info, error)
	// List 返回 Scope 内全部 Node（含离线），按标签排序。
	List(ctx context.Context, scope Scope) ([]*Info, error)
	// DeleteScope 删除 Scope 内全部 Node 的登记（EndUser 注销），返回被删除的 Node ID。
	DeleteScope(ctx context.Context, scope Scope) ([]string, error)
}

// Inbox 保存每个 Node 待投递的 Invocation。
type Inbox interface {
	// Put 按 call_id 幂等地加入。
	Put(ctx context.Context, nodeID string, inv *v1.Invoke) error
	Remove(ctx context.Context, nodeID, callID string) error
	// RemoveSession 从所有 Node 的 Inbox 中撤回属于该 Session 的调用（ADR-0015）。
	RemoveSession(ctx context.Context, sessionID string) error
	// Pending 返回当前全部待投递项（按加入顺序）与版本号。
	Pending(ctx context.Context, nodeID string) ([]*v1.Invoke, uint64, error)
	// Wait 阻塞直到版本号不再等于 version，或 ctx 结束。
	Wait(ctx context.Context, nodeID string, version uint64) error
}

// Waker 唤醒离线 Node 所在的 HostApp（移动推送等）。
type Waker interface {
	Wake(ctx context.Context, info *Info)
}

type Identity struct {
	BusinessLine string
	EndUser      string
	// Expires 是凭证到期时间，网关在此刻断开连接；零值表示不过期。
	Expires time.Time
}

type Authenticator interface {
	Authenticate(ctx context.Context, hello *v1.Hello) (Identity, error)
}

// TokenAuth 以 Hello.token 中业务线签发的用户令牌确定身份（docs/design/auth.md §3）。
// Hello 中自报的业务线与 EndUser 只能为空或与令牌一致；服务令牌不能接入 Node。
type TokenAuth struct{ Verifier auth.Verifier }

func (a TokenAuth) Authenticate(_ context.Context, h *v1.Hello) (Identity, error) {
	p, err := a.Verifier.Verify(h.GetToken())
	if err != nil {
		return Identity{}, err
	}
	if p.Service() {
		return Identity{}, errors.New("a node must act for an end user; service tokens are not accepted")
	}
	if (h.GetBusinessLine() != "" && h.GetBusinessLine() != p.BusinessLine) || (h.GetEndUser() != "" && h.GetEndUser() != p.EndUser) {
		return Identity{}, errors.New("hello identity does not match token")
	}
	return Identity{BusinessLine: p.BusinessLine, EndUser: p.EndUser, Expires: p.Expires}, nil
}

// InsecureDevAuth 信任 Hello 中自报的身份，仅用于开发（serve -auth none，仅限回环地址）。
type InsecureDevAuth struct{}

func (InsecureDevAuth) Authenticate(_ context.Context, h *v1.Hello) (Identity, error) {
	if h.GetBusinessLine() == "" || h.GetEndUser() == "" {
		return Identity{}, errors.New("business_line and end_user are required")
	}
	return Identity{BusinessLine: h.GetBusinessLine(), EndUser: h.GetEndUser()}, nil
}

// reservedPrefixes 是平台保留的 Node ID 前缀：沙箱的虚拟 Node（"sbx_<Session ID>"，ADR-0008）与用户本人
// （ask_user 的 "@user"，ADR-0025）。
var reservedPrefixes = []string{"sbx_", "@"}

// Reserved 报告 Node ID 是否属于平台。设备不能以这些 ID 接入：否则持有合法令牌的设备可以收到他人沙箱的调用
// （含参数）并回写伪造的结果，或让自己声明的能力被当作沙箱调用执行，读到他人的工作区。
func Reserved(nodeID string) bool {
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(nodeID, p) {
			return true
		}
	}
	return false
}

var labelInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// sandboxLabel 是沙箱工具名的前缀部分（capability.SandboxLabel）：设备不能用它作标签，否则其工具名
// 与沙箱工具相同，审批摘要会把设备上的调用说成"在云端"。
const sandboxLabel = "sandbox"

// SanitizeLabel 把标签规范为 [a-z0-9-]：不含下划线，使工具名 "<label>__<capability>" 可无歧义拆分。
func SanitizeLabel(s string) string {
	s = strings.Trim(labelInvalid.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 24 {
		s = s[:24]
	}
	switch s {
	case "":
		s = "device"
	case sandboxLabel:
		s = sandboxLabel + "-device"
	}
	return s
}
