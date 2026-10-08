package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/node"
)

// AgentDef 白名单中的前缀条目：
//
//	"device:<glob>"   EndUser 设备上名称匹配的能力，如 "device:*"
//	"sandbox:<glob>"  本 Session 云端沙箱中名称匹配的能力（docs/design/m2-sandbox.md）
const (
	DevicePrefix  = "device:"
	SandboxPrefix = "sandbox:"
	// SandboxLabel 是沙箱工具名的前缀部分："sandbox__<capability>"。
	SandboxLabel = "sandbox"
)

// Target 是解析工具时的上下文：Scope 决定可见的设备，SessionID 决定沙箱。
type Target struct {
	Scope     node.Scope
	SessionID string
}

// SandboxTools 配置沙箱能力；Catalog.Sandbox 为 nil 时不提供沙箱。
type SandboxTools struct {
	Specs  []*v1.CapabilitySpec
	NodeID func(sessionID string) string
}

// Tool 是面向模型的一个工具及其执行目标：Local 非空为进程内调用，否则路由到 NodeID。
type Tool struct {
	// Spec.Name 是面向模型的工具名；路由工具为 "<label>__<capability>"。
	Spec  Spec
	Local Capability

	NodeID     string
	Capability string
	Timeout    time.Duration
}

func (t *Tool) RequiresApproval() bool { return t.Spec.Risk == v1.Risk_RISK_HIGH }

// Catalog 解析某个 Session 可用的工具（能力路由）。
type Catalog struct {
	Local *Registry
	// Nodes 为 nil 时没有设备能力。
	Nodes   node.Directory
	Sandbox *SandboxTools
	// DefaultTimeout 用于未声明超时的设备能力。
	DefaultTimeout time.Duration
}

// Tools 返回 allow 白名单允许、对 target 可见的全部工具：进程内工具、沙箱工具、设备工具（按标签排序）。
func (c *Catalog) Tools(ctx context.Context, target Target, allow []string) ([]Tool, error) {
	var out []Tool
	var globs, sandboxGlobs []string
	for _, a := range allow {
		if g, ok := strings.CutPrefix(a, DevicePrefix); ok {
			if _, err := path.Match(g, ""); err != nil {
				return nil, fmt.Errorf("bad capability pattern %q: %w", a, err)
			}
			globs = append(globs, g)
			continue
		}
		if g, ok := strings.CutPrefix(a, SandboxPrefix); ok {
			if _, err := path.Match(g, ""); err != nil {
				return nil, fmt.Errorf("bad capability pattern %q: %w", a, err)
			}
			sandboxGlobs = append(sandboxGlobs, g)
			continue
		}
		cp, ok := c.Local.Get(a)
		if !ok {
			return nil, fmt.Errorf("unknown capability %q", a)
		}
		out = append(out, Tool{Spec: cp.Spec(), Local: cp})
	}
	if c.Sandbox != nil && target.SessionID != "" {
		for _, cs := range c.Sandbox.Specs {
			if matchAny(sandboxGlobs, cs.GetName()) {
				out = append(out, Tool{
					Spec:   Spec{Name: SandboxLabel + "__" + cs.GetName(), Description: cs.GetDescription(), InputSchema: json.RawMessage(cs.GetInputSchemaJson()), Idempotent: cs.GetIdempotent(), Risk: cs.GetRisk()},
					NodeID: c.Sandbox.NodeID(target.SessionID), Capability: cs.GetName(),
					Timeout: time.Duration(cs.GetTimeoutSeconds()) * time.Second,
				})
			}
		}
	}
	if len(globs) == 0 || c.Nodes == nil {
		return out, nil
	}
	nodes, err := c.Nodes.List(ctx, target.Scope)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		for _, cs := range n.Capabilities {
			if !matchAny(globs, cs.GetName()) {
				continue
			}
			out = append(out, c.deviceTool(n, cs))
		}
	}
	return out, nil
}

// Resolve 按工具名解析；不存在时返回 nil。
func (c *Catalog) Resolve(ctx context.Context, target Target, allow []string, name string) (*Tool, error) {
	tools, err := c.Tools(ctx, target, allow)
	if err != nil {
		return nil, err
	}
	for i := range tools {
		if tools[i].Spec.Name == name {
			return &tools[i], nil
		}
	}
	return nil, nil
}

func matchAny(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

func (c *Catalog) deviceTool(n *node.Info, cs *v1.CapabilitySpec) Tool {
	state := "在线"
	if !n.Online {
		state = "当前离线，调用会等待其上线"
	}
	schema := json.RawMessage(cs.GetInputSchemaJson())
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	timeout := time.Duration(cs.GetTimeoutSeconds()) * time.Second
	if timeout == 0 {
		timeout = c.DefaultTimeout
	}
	return Tool{
		Spec: Spec{
			Name:        n.Label + "__" + cs.GetName(),
			Description: fmt.Sprintf("[设备 %s（%s）· %s] %s", n.Label, n.Kind, state, cs.GetDescription()),
			InputSchema: schema,
			Idempotent:  cs.GetIdempotent(),
			Risk:        cs.GetRisk(),
		},
		NodeID: n.NodeID, Capability: cs.GetName(), Timeout: timeout,
	}
}
