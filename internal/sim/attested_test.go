package sim

import (
	"context"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/model"
)

// 从 MCP 登记到模型可见的渲染文字走通，防止脚本仍等候 Transcript 已展开掉的 capture 内容块。
func TestAttestedModelContinuesWithMemory(t *testing.T) {
	w, err := New(Options{Seed: 1, Attested: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.attested.Close()
	ctx := context.Background()
	tools, err := w.attested.Tools(ctx, "bl", []string{"sim/pages"})
	if err != nil || len(tools) != 1 {
		t.Fatalf("MCP tools: %d, %v", len(tools), err)
	}
	inv := capability.Invocation{BusinessLine: "bl", EndUser: "u", SessionID: "ses_v1_000_000_s", CallID: "page", Arguments: `{}`}
	out, err := tools[0].Local.(capability.ResultInvoker).InvokeResult(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	page, err := capture.ParsePage(out.Page, capture.OptionsFor(inv))
	if err != nil {
		t.Fatal(err)
	}
	blocks, _, err := capture.Render(page, 0)
	if err != nil {
		t.Fatal(err)
	}
	req := &model.Request{Tools: []model.ToolSpec{{Name: simAttestedName}, {Name: "memory_save"}}, Messages: []model.Message{
		{Role: model.RoleAssistant, ToolCalls: []*v1.ToolCall{{CallId: "page", Capability: simAttestedName}}},
		{Role: model.RoleTool, ToolCallID: "page", Content: model.ExpandCaptures(blocks)},
	}}
	resp, err := (&simModel{w: w}).Generate(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || !memoryPrecheckCall(resp.ToolCalls[0]) {
		t.Fatalf("page did not lead to the memory precheck sample: %+v", resp.ToolCalls)
	}
	// 结果不可用或全部拦截时，不能冒充模型已经读到他人的原话。
	for _, g := range page.Segments {
		g.Text, g.Redacted = "", true
	}
	blocks, _, err = capture.Render(page, 0)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages[1].Content = model.ExpandCaptures(blocks)
	if attestedMemoryPrecheck(req.Messages) {
		t.Fatal("redacted page counted as readable other speech")
	}
	req.Messages[1].Content = model.TextBlocks("[error] page unavailable")
	if attestedMemoryPrecheck(req.Messages) {
		t.Fatal("failed page counted as readable other speech")
	}
}
