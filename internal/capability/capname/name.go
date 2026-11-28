// Package capname 是内核能力命名约定的唯一出处。Session 的来源推导依赖这些约定，
// 生成名称与判断名称必须共用它们，才能按原语义解释已有 Event。
package capname

import (
	"regexp"
	"strings"
)

const (
	Separator            = "__"
	AgentLabel           = "agent"
	SandboxLabel         = "sandbox"
	DelegationToolPrefix = AgentLabel + Separator
	MCPToolPrefix        = "mcp_"
	ScheduleInputPrefix  = "schedule_"

	SandboxNodePrefix    = "sbx_"
	platformNodePrefix   = "@"
	DelegationNodePrefix = platformNodePrefix + "agent/"
	UserNodeID           = platformNodePrefix + "user"

	AskUserCapability    = "ask_user"
	ShowCardCapability   = "show_card"
	MemorySaveCapability = "memory_save"
)

// Routed 生成 Catalog 暴露给模型的路由工具名。
func Routed(label, name string) string { return label + Separator + name }

// IsRouted 用于来源推导，已有 Event 中任何含分隔符的工具名都按外部内容处理。
func IsRouted(name string) bool { return strings.Contains(name, Separator) }

// CutRouted 只切开第一个分隔符，保留能力名中可能还含有的分隔符。
func CutRouted(name string) (label, capability string, ok bool) {
	return strings.Cut(name, Separator)
}

func IsDelegation(name string) bool { return strings.HasPrefix(name, DelegationToolPrefix) }

func CutDelegation(name string) (agent string, ok bool) {
	return strings.CutPrefix(name, DelegationToolPrefix)
}

func IsSandboxTool(name string) bool { return strings.HasPrefix(name, SandboxLabel+Separator) }

// IsReservedLabel 保留沙箱与委托的工具命名，避免设备工具被当作平台工具。
func IsReservedLabel(label string) bool { return label == SandboxLabel || label == AgentLabel }

const (
	mcpServerPattern = `[a-z0-9][a-z0-9-]{0,23}`
	MCPServerPattern = `^` + mcpServerPattern + `$`
)

var (
	mcpServerName = regexp.MustCompile(MCPServerPattern)
	mcpToolName   = regexp.MustCompile(`^` + MCPToolPrefix + mcpServerPattern + Separator + `[a-zA-Z0-9_-]+$`)
)

func IsMCPServer(name string) bool { return mcpServerName.MatchString(name) }

// IsMCP 判断日志中的完整 MCP 工具名，Apply 用它限制 attested 结果的来源。
func IsMCP(name string) bool { return mcpToolName.MatchString(name) }

func IsScheduleInput(id string) bool { return strings.HasPrefix(id, ScheduleInputPrefix) }

func SandboxNodeID(sessionID string) string { return SandboxNodePrefix + sessionID }

func IsSandboxNode(id string) bool { return strings.HasPrefix(id, SandboxNodePrefix) }

func SandboxSession(id string) string { return strings.TrimPrefix(id, SandboxNodePrefix) }

func DelegationNodeID(sessionID string) string { return DelegationNodePrefix + sessionID }

func IsDelegationNode(id string) bool { return strings.HasPrefix(id, DelegationNodePrefix) }

func DelegationSession(id string) string { return strings.TrimPrefix(id, DelegationNodePrefix) }

// IsReservedNode 拒绝设备以平台 Node ID 接入，包括整个 @ 前缀下的 ID。
func IsReservedNode(id string) bool {
	return IsSandboxNode(id) || strings.HasPrefix(id, platformNodePrefix)
}
