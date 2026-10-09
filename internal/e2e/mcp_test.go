package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/mcpcap"
	"yanshi/sdk/nodesdk"
)

// TestRemoteMCPToolNeedsApproval：远程 MCP Server 中未声明只读的工具，经审批后才执行（docs/design/m5-mcp.md §3）。
func TestRemoteMCPToolNeedsApproval(t *testing.T) {
	var calls atomic.Int32
	srv := mcp.NewServer(&mcp.Implementation{Name: "crm", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "create_ticket", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "工单 T-1"}}}, nil
		})
	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer hs.Close()

	st := memStores()
	conn := &mcpcap.Connector{Servers: []mcpcap.Server{{Name: "crm", BusinessLine: "bl", URL: hs.URL, Timeout: 5 * time.Second}}, Artifacts: st.artifacts}
	defer conn.Close()
	st.mcp = conn
	e := &env{t: t, srv: instance(t, st, 2, true)}
	sid := e.newSession()
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": `call mcp_crm__create_ticket {"title":"打印机坏了"}`}, nil)
	v := e.until(sid, func(v sessionView) bool {
		return len(v.Runs) == 1 && v.Runs[0].Waiting != nil && v.Runs[0].Waiting.Kind == "approval"
	})
	if calls.Load() != 0 {
		t.Fatal("executed before approval")
	}
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/approvals/"+v.Runs[0].Waiting.CallID, map[string]bool{"approve": true}, nil)
	e.until(sid, func(v sessionView) bool { st, _ := lastRun(v); return st == "completed" })
	if got := e.lastAssistantText(sid); got != "done: 工单 T-1" || calls.Load() != 1 {
		t.Fatalf("assistant = %q, calls = %d", got, calls.Load())
	}
}

// TestDeviceMCPBridge：电脑端 SDK 把本机 MCP Server 桥接为设备能力，经 Inbox 路由执行（§5）。
func TestDeviceMCPBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local := mcp.NewServer(&mcp.Implementation{Name: "notes", Version: "1"}, nil)
	local.AddTool(&mcp.Tool{Name: "read_note", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "本机笔记"}}}, nil
		})
	ct, stp := mcp.NewInMemoryTransports()
	if _, err := local.Connect(ctx, stp, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "node", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	caps, err := nodesdk.MCPCapabilities(ctx, "notes", cs, nil)
	if err != nil {
		t.Fatal(err)
	}

	e := setup(t)
	connected := make(chan struct{}, 1)
	c := nodesdk.NewClient(nodesdk.Config{
		URL: "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/nodes/connect", NodeID: "node-mcp",
		TokenSource: userToken, Label: "laptop", Kind: "desktop", Executor: nodesdk.NewExecutor(nodesdk.NewMemLedger(), caps...),
		OnConnected: func(string) { connected <- struct{}{} },
	})
	go func() { _ = c.Run(ctx) }()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("node did not connect")
	}
	sid := e.newSession()
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "call laptop__notes_read_note"}, nil)
	e.until(sid, func(v sessionView) bool { st, _ := lastRun(v); return st == "completed" })
	if got := e.lastAssistantText(sid); got != "done: 本机笔记" {
		t.Fatalf("assistant = %q", got)
	}
}
