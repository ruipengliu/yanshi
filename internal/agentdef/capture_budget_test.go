package agentdef_test

import (
	"strings"
	"testing"

	"yanshi/internal/agentdef"
	"yanshi/internal/session"
)

func TestCaptureResultBudget(t *testing.T) {
	minBudget := session.MinCaptureToolResult()
	for _, tc := range []struct {
		name           string
		window, budget int
		captures, fail bool
	}{
		{"default", 0, 0, true, false},
		{"exact", 64000, minBudget, true, false},
		{"below", 64000, minBudget - 1, true, true},
		{"small default window", 8000, 0, true, true},
		{"without captures", 8000, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &agentdef.Def{Name: "test", Version: "1", Model: "test/model", Context: agentdef.Context{Window: tc.window, MaxToolResult: tc.budget}}
			if tc.captures {
				d.Capabilities = []string{session.CapturesCapability}
			}
			_, err := agentdef.NewRegistry(d)
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v", err)
			}
			if tc.fail && (!strings.Contains(err.Error(), "captures") || !strings.Contains(err.Error(), "context.max_tool_result")) {
				t.Fatal(err)
			}
		})
	}
}

func TestRepositoryAgentsLoad(t *testing.T) {
	if _, err := agentdef.LoadDir("../../agents"); err != nil {
		t.Fatal(err)
	}
}
