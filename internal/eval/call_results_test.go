package eval

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

func TestCallResultsUseCallIDAndFullText(t *testing.T) {
	call := func(id, capability string) *v1.ToolCall {
		return &v1.ToolCall{CallId: id, Capability: capability, ArgumentsJson: `{}`}
	}
	result := func(id, text string, isError bool) *v1.Event {
		return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{CallId: id, Content: model.TextBlocks(text), IsError: isError}}}
	}
	obs := &observation{status: "completed"}
	collect([]*v1.Event{
		{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{ToolCalls: []*v1.ToolCall{call("a", "mcp_xinli__file_changes"), call("b", "unrelated"), call("c", "mcp_xinli__file_changes")}}}},
		result("b", "目标回执", false),
		result("c", "已经接受", false),
		result("a", strings.Repeat("前", judgeStepBytes)+"目标回执目标回执", true),
		result("missing", "目标回执", false),
	}, obs)
	for _, step := range obs.steps[3:] {
		if strings.HasPrefix(step, "错误") && strings.Contains(step, "目标回执") {
			t.Fatal("test did not exercise judge truncation")
		}
	}
	zero, one, two := 0, 1, 2
	turn := Turn{Expect: Expect{CallResults: []CallResultMatch{
		{Capability: "mcp_xinli__*", Contains: "目标回执", Count: &Range{Min: &one, Max: &one}},
		{Capability: "mcp_xinli__file_changes", Contains: "已经接受"},
		{Capability: "mcp_xinli__file_changes", Contains: "目标回执", Count: &Range{Min: &two}},
		{Capability: "never", Contains: "目标回执", Count: &Range{Min: &zero, Max: &zero}},
		{Capability: "never", Contains: "目标回执"},
		{Capability: "mcp_xinli__file_changes", Contains: "", Count: &Range{Max: &one}},
	}}}
	as, _, _ := (&instance{}).check(context.Background(), 1, turn, obs)
	var passes []bool
	for _, a := range as {
		if strings.HasPrefix(a.Name, "call_results ") {
			passes = append(passes, a.Pass)
			if !a.Pass && (!strings.Contains(a.Detail, "完整结果") || !strings.Contains(a.Detail, "要求")) {
				t.Fatalf("unreadable failure: %+v", a)
			}
		}
	}
	if !reflect.DeepEqual(passes, []bool{true, true, false, true, false, false}) {
		t.Fatalf("call result assertions = %v", passes)
	}
	// 拆 Run 后仍按结果条数计数，同一条结果内的两次子串不算两条。
	combined := &observation{}
	combined.appendRun(obs)
	combined.appendRun(&observation{callResults: []observedCallResult{{callID: "next", capability: "mcp_xinli__file_changes", text: "目标回执"}}})
	checkCallResults(func(_ string, pass bool, detail string) {
		if !pass {
			t.Fatal(detail)
		}
	}, []CallResultMatch{{Capability: "mcp_xinli__file_changes", Contains: "目标回执", Count: &Range{Min: &two, Max: &two}}}, combined.callResults)
}

func TestCallResultsYAML(t *testing.T) {
	for _, tc := range []struct {
		name, assertion string
		valid           bool
	}{
		{"defaults", `{capability: mcp_xinli__*, contains: 拒收}`, true},
		{"zero", `{capability: mcp_xinli__*, contains: 拒收, count: {min: 0, max: 0}}`, true},
		{"missing capability", `{contains: 拒收}`, false},
		{"bad glob", `{capability: "[", contains: 拒收}`, false},
		{"negative min", `{capability: x, count: {min: -1}}`, false},
		{"negative max", `{capability: x, count: {min: 0, max: -1}}`, false},
		{"inverted", `{capability: x, count: {min: 2, max: 1}}`, false},
		{"default min", `{capability: x, count: {max: 0}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCase(t, dir, "result", "name: result\nturns:\n  - input: hi\n    expect:\n      call_results: ["+tc.assertion+"]\n")
			_, err := LoadSuite(dir)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
		})
	}
}
