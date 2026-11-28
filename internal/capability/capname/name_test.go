package capname

import (
	"strings"
	"testing"
)

// 旧 Event 中的字符串也参与投影，判定须保留对不完整名称的处理。
func TestToolNamePredicates(t *testing.T) {
	for _, tt := range []struct {
		name       string
		routed     bool
		delegation bool
		mcp        bool
	}{
		{name: ""},
		{name: "ask_user"},
		{name: "__", routed: true},
		{name: "agent__", routed: true, delegation: true},
		{name: "notagent__helper", routed: true},
		{name: "mcp_crm__", routed: true},
		{name: "mcp_CRM__read", routed: true},
		{name: "mcp:crm/read"},
		{name: "mcp_" + strings.Repeat("a", 24) + "__Read-1", routed: true, mcp: true},
		{name: "mcp_" + strings.Repeat("a", 25) + "__read", routed: true},
		{name: "mcp_crm__" + strings.Repeat("a", 64), routed: true, mcp: true},
	} {
		if IsRouted(tt.name) != tt.routed || IsDelegation(tt.name) != tt.delegation || IsMCP(tt.name) != tt.mcp {
			t.Errorf("%q: routed=%t, delegation=%t, MCP=%t", tt.name, IsRouted(tt.name), IsDelegation(tt.name), IsMCP(tt.name))
		}
	}
	if label, name, ok := CutRouted("phone__read__file"); label != "phone" || name != "read__file" || !ok {
		t.Errorf("CutRouted = %q, %q, %t", label, name, ok)
	}
	if label, name, ok := CutRouted("read_file"); label != "read_file" || name != "" || ok {
		t.Errorf("CutRouted without separator = %q, %q, %t", label, name, ok)
	}
}

func TestReservedNames(t *testing.T) {
	if id := SandboxNodeID("child"); id != "sbx_child" || !IsSandboxNode(id) || !IsReservedNode(id) || SandboxSession(id) != "child" {
		t.Errorf("sandbox node ID = %q", id)
	}
	if id := DelegationNodeID("child"); id != "@agent/child" || !IsDelegationNode(id) || !IsReservedNode(id) || DelegationSession(id) != "child" {
		t.Errorf("delegation node ID = %q", id)
	}
	if !IsReservedNode("@other") || IsReservedNode("xsbx_child") || IsDelegationNode("@agent") {
		t.Error("reserved node prefixes changed")
	}
	if !IsScheduleInput("schedule_") || IsScheduleInput("schedule") || IsScheduleInput("x_schedule_123") {
		t.Error("reserved schedule input prefix changed")
	}
}
