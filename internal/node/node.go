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
}

// Inbox 保存每个 Node 待投递的 Invocation。
type Inbox interface {
	// Put 按 call_id 幂等地加入。
	Put(ctx context.Context, nodeID string, inv *v1.Invoke) error
	Remove(ctx context.Context, nodeID, callID string) error
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
}

type Authenticator interface {
	Authenticate(ctx context.Context, hello *v1.Hello) (Identity, error)
}

// InsecureDevAuth 信任 Hello 中自报的身份，仅用于开发（m1 设计 §7）。
type InsecureDevAuth struct{}

func (InsecureDevAuth) Authenticate(_ context.Context, h *v1.Hello) (Identity, error) {
	if h.GetBusinessLine() == "" || h.GetEndUser() == "" {
		return Identity{}, errors.New("business_line and end_user are required")
	}
	return Identity{BusinessLine: h.GetBusinessLine(), EndUser: h.GetEndUser()}, nil
}

var labelInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// SanitizeLabel 把标签规范为 [a-z0-9-]：不含下划线，使工具名 "<label>__<capability>" 可无歧义拆分。
func SanitizeLabel(s string) string {
	s = strings.Trim(labelInvalid.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 24 {
		s = s[:24]
	}
	if s == "" {
		s = "device"
	}
	return s
}
