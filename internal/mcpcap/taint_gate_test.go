package mcpcap

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/capability"
)

func TestTaintGatedApprovalSummary(t *testing.T) {
	truth := true
	const shortArgs = `{"q":"客户"}`
	const summaryPrefix = "在云端上执行 mcp_crm__create_ticket，参数 "
	for _, tc := range []struct {
		name        string
		noApproval  []string
		taintGated  []string
		annotations *mcp.ToolAnnotations
		approval    bool
		summary     string
	}{
		{"relaxed and gated", []string{"create_*"}, []string{"*_ticket"}, nil, false, summaryPrefix + shortArgs + taintGatedSuffix},
		{"only relaxed", []string{"create_*"}, nil, nil, false, ""},
		{"glob does not match", []string{"create_*"}, []string{"update_*"}, nil, false, ""},
		{"not relaxed", nil, []string{"create_*"}, nil, true, ""},
		{"destructive", []string{"create_*"}, []string{"create_*"}, &mcp.ToolAnnotations{DestructiveHint: &truth}, true, ""},
		{"read only", []string{"create_*"}, []string{"create_*"}, &mcp.ToolAnnotations{ReadOnlyHint: true}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Server{Name: "crm"}
			s.Tools.NoApproval = tc.noApproval
			s.Tools.TaintGated = tc.taintGated
			tool := (&Connector{}).tool(s, &mcp.Tool{Name: "create_ticket", Annotations: tc.annotations})
			if got := tool.RequiresApproval(); got != tc.approval {
				t.Errorf("RequiresApproval() = %t, want %t", got, tc.approval)
			}
			summarizer, ok := tool.Local.(capability.ApprovalSummarizer)
			if !ok {
				t.Fatal("remote tool does not implement ApprovalSummarizer")
			}
			if got := summarizer.ApprovalSummary(capability.Invocation{Arguments: shortArgs}); got != tc.summary {
				t.Errorf("ApprovalSummary() = %q, want %q", got, tc.summary)
			}
			if tc.summary != "" {
				args := `{"q":"` + strings.Repeat("界", 70) + `"}`
				want := summaryPrefix + `{"q":"` + strings.Repeat("界", 64) + "…" + taintGatedSuffix
				if got := summarizer.ApprovalSummary(capability.Invocation{Arguments: args}); got != want {
					t.Errorf("ApprovalSummary() with long arguments = %q, want %q", got, want)
				}
			}
		})
	}
}
