package mcpcap

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReadOnlyAnnotation(t *testing.T) {
	truth := true
	for _, tc := range []struct {
		name        string
		annotations *mcp.ToolAnnotations
		want        bool
	}{
		{"absent", nil, false},
		{"false", &mcp.ToolAnnotations{ReadOnlyHint: false}, false},
		{"idempotent", &mcp.ToolAnnotations{IdempotentHint: true}, false},
		{"destructive", &mcp.ToolAnnotations{DestructiveHint: &truth}, false},
		{"read only", &mcp.ToolAnnotations{ReadOnlyHint: true}, true},
		// 注解冲突也不能改成从风险推导；只读证据严格取 ReadOnlyHint。
		{"read only and destructive", &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &truth}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := Server{Name: "test"}
			server.Tools.NoApproval = []string{"tool"}
			tool := (&Connector{}).tool(server, &mcp.Tool{Name: "tool", Annotations: tc.annotations})
			if tool.Spec.ReadOnly != tc.want || tool.Local.Spec().ReadOnly != tc.want {
				t.Fatalf("ReadOnly = %v (local %v), want %v", tool.Spec.ReadOnly, tool.Local.Spec().ReadOnly, tc.want)
			}
		})
	}
}
