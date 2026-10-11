package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/clock"
	"yanshi/internal/node"
)

// AgentDef 白名单中的前缀条目：
//
//	"device:<glob>"   EndUser 设备上名称匹配的能力，如 "device:*"
//	"sandbox:<glob>"  本 Session 云端沙箱中名称匹配的能力（docs/design/m2-sandbox.md）
const (
	// MCPPrefix 标记远程 MCP 工具："mcp:<server>/<glob>"。
	MCPPrefix     = "mcp:"
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
	// MCP 为 nil 时没有远程 MCP 工具（docs/design/m5-mcp.md）。
	MCP MCPSource
	// DefaultTimeout 用于未声明超时的设备能力。
	DefaultTimeout time.Duration
	// AskTimeout 是 ask_user 等待回答的上限，默认 askuser.DefaultTimeout（ADR-0025）。
	AskTimeout time.Duration
	// NodeCacheTTL > 0 时缓存每个 EndUser 的 Node 列表（时间取自 Clock）：每次模型调用与每个调用的每个阶段都要解析
	// 工具，不缓存时每次都查一次 Node 目录（延展性评审 §4.3）。代价是新登记的设备、变化的能力至多晚 TTL 可见；
	// 工具不含在线状态（见 deviceTool），设备上下线不受影响。
	NodeCacheTTL time.Duration
	Clock        clock.Clock

	mu    sync.Mutex
	nodes map[node.Scope]cachedNodes
}

type cachedNodes struct {
	list []*node.Info
	at   time.Time
}

// listNodes 返回 scope 的 Node 列表，按 NodeCacheTTL 缓存。
func (c *Catalog) listNodes(ctx context.Context, scope node.Scope) ([]*node.Info, error) {
	if c.NodeCacheTTL <= 0 {
		return c.Nodes.List(ctx, scope)
	}
	now := c.Clock.Now()
	c.mu.Lock()
	if e, ok := c.nodes[scope]; ok && now.Sub(e.at) < c.NodeCacheTTL {
		c.mu.Unlock()
		return e.list, nil
	}
	c.mu.Unlock()
	list, err := c.Nodes.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nodes == nil {
		c.nodes = map[node.Scope]cachedNodes{}
	}
	if len(c.nodes) >= 4096 {
		for k, e := range c.nodes {
			if now.Sub(e.at) >= c.NodeCacheTTL {
				delete(c.nodes, k)
			}
		}
	}
	c.nodes[scope] = cachedNodes{list: list, at: now}
	return list, nil
}

// Tools 返回 allow 白名单允许、对 target 可见的全部工具：进程内工具、沙箱工具、设备工具（按标签排序）。
func (c *Catalog) Tools(ctx context.Context, target Target, allow []string) ([]Tool, error) {
	var out []Tool
	var globs, sandboxGlobs, mcpPatterns []string
	for _, a := range allow {
		if p, ok := strings.CutPrefix(a, MCPPrefix); ok {
			mcpPatterns = append(mcpPatterns, p)
			continue
		}
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
		if a == askuser.Capability {
			out = append(out, c.askTool())
			continue
		}
		cp, ok := c.Local.Get(a)
		if !ok {
			return nil, fmt.Errorf("unknown capability %q", a)
		}
		out = append(out, Tool{Spec: cp.Spec(), Local: cp})
	}
	if c.MCP != nil && len(mcpPatterns) > 0 {
		tools, err := c.MCP.Tools(ctx, target.Scope.BusinessLine, mcpPatterns)
		if err != nil {
			return nil, err
		}
		out = append(out, byName(tools)...)
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
	nodes, err := c.listNodes(ctx, target.Scope)
	if err != nil {
		return nil, err
	}
	var devices []Tool
	for _, n := range nodes {
		if node.Reserved(n.NodeID) || n.Label == SandboxLabel {
			continue // 平台的 ID 与标签不属于设备（node.Hub.Connect 已拒绝，这里防御此前写入的登记）
		}
		for _, cs := range n.Capabilities {
			if !matchAny(globs, cs.GetName()) {
				continue
			}
			devices = append(devices, c.deviceTool(n, cs))
		}
	}
	return append(out, byName(devices)...), nil
}

// askTool 是 ask_user：路由到用户本人的调用（ADR-0025），不经 Inbox。
func (c *Catalog) askTool() Tool {
	timeout := c.AskTimeout
	if timeout == 0 {
		timeout = askuser.DefaultTimeout
	}
	return Tool{
		Spec: Spec{Name: askuser.Capability, Description: askuser.Description, InputSchema: json.RawMessage(askuser.InputSchema),
			Idempotent: true, Risk: v1.Risk_RISK_LOW},
		NodeID: askuser.NodeID, Capability: askuser.Capability, Timeout: timeout,
	}
}

// byName 按名称排序：工具定义是请求前缀的一部分，顺序不能随 Node 列表或 MCP Server 的返回顺序变化，
// 否则提供商的前缀缓存失效。
func byName(tools []Tool) []Tool {
	slices.SortFunc(tools, func(a, b Tool) int { return strings.Compare(a.Spec.Name, b.Spec.Name) })
	return tools
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

// deviceTool 是设备能力对应的工具。描述只含设备的标签与类型，不含在线状态：工具定义在请求前缀中，手机等设备
// 频繁上下线会使同一 Run 中之后每次请求的前缀缓存失效。调用离线设备时 Run 挂起，设备被唤醒、上线后继续。
func (c *Catalog) deviceTool(n *node.Info, cs *v1.CapabilitySpec) Tool {
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
			Description: fmt.Sprintf("[设备 %s（%s）] %s", n.Label, n.Kind, cs.GetDescription()),
			InputSchema: schema,
			Idempotent:  cs.GetIdempotent(),
			Risk:        cs.GetRisk(),
		},
		NodeID: n.NodeID, Capability: cs.GetName(), Timeout: timeout,
	}
}

// MCPSource 提供业务线登记的远程 MCP 工具（mcpcap.Connector）。patterns 已去掉 "mcp:" 前缀。
type MCPSource interface {
	Tools(ctx context.Context, businessLine string, patterns []string) ([]Tool, error)
}
