package nodesdk_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
)

// localServer 是一个内存中的本机 MCP Server：一个只读工具、一个有副作用的工具、一个返回图片的工具。
func localServer(t *testing.T) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "local", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "read_note", Description: "读笔记", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct{ Name string }
			_ = json.Unmarshal(req.Params.Arguments, &in)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "笔记 " + in.Name}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "send.mail", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "mailbox unavailable"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "screenshot", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: []byte("png-bytes")}}}, nil
		})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMCPBridgeCapabilities(t *testing.T) {
	ctx := context.Background()
	caps, err := nodesdk.MCPCapabilities(ctx, "notes", localServer(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]nodesdk.Capability{}
	for _, c := range caps {
		byName[c.Spec.GetName()] = c
	}
	read, ok := byName["notes_read_note"]
	if !ok || read.Spec.GetRisk() != v1.Risk_RISK_LOW || !read.Spec.GetIdempotent() || !strings.Contains(read.Spec.GetInputSchemaJson(), "name") {
		t.Fatalf("read_note = %+v (all: %v)", read.Spec, byName)
	}
	var send nodesdk.Capability
	for n, c := range byName {
		if strings.HasPrefix(n, "notes_send_mail_") {
			send = c
		}
	}
	if send.Spec == nil || send.Spec.GetRisk() != v1.Risk_RISK_HIGH || send.Spec.GetIdempotent() {
		t.Fatalf("a tool without annotations must require approval: %+v", send.Spec)
	}

	// 经 Executor 执行：与其他设备能力相同的去重路径。
	exec := nodesdk.NewExecutor(nodesdk.NewMemLedger(), caps...)
	res, err := exec.Execute(ctx, &v1.Invoke{SessionId: "s", CallId: "c1", Capability: "notes_read_note", ArgumentsJson: `{"name":"周报"}`})
	if err != nil || res.GetIsError() || res.GetContent()[0].GetText().GetText() != "笔记 周报" {
		t.Fatalf("read = %v, %v", res, err)
	}
	res, _ = exec.Execute(ctx, &v1.Invoke{SessionId: "s", CallId: "c2", Capability: send.Spec.GetName(), ArgumentsJson: `{}`})
	if !res.GetIsError() || !strings.Contains(res.GetContent()[0].GetText().GetText(), "mailbox unavailable") {
		t.Fatalf("tool error not surfaced: %v", res)
	}
	res, _ = exec.Execute(ctx, &v1.Invoke{SessionId: "s", CallId: "c3", Capability: "notes_screenshot", ArgumentsJson: `{}`})
	if !strings.Contains(res.GetContent()[0].GetText().GetText(), "image") {
		t.Fatalf("binary content without an uploader should be described: %v", res)
	}
}

func TestSafeName(t *testing.T) {
	if got := nodesdk.SafeName("read_note", 36); got != "read_note" {
		t.Fatalf("valid name changed: %q", got)
	}
	a, b := nodesdk.SafeName("a.b", 36), nodesdk.SafeName("a_b", 36)
	if a == b || len(nodesdk.SafeName(strings.Repeat("x", 100), 20)) > 20 {
		t.Fatalf("a.b → %q, a_b → %q", a, b)
	}
}
