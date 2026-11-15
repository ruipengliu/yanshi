package runtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
	"yanshi/internal/runtime"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

// 验证真实 MCP 到 Worker 的登记边界；未登记的页仍是普通外部结果。
func TestMCPPageWrittenByWorker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared bool
		value    any
		want     bool
	}{
		{"accepted", true, map[string]string{"kind": "empty"}, true},
		{"undeclared", false, map[string]string{"kind": "empty"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv := mcp.NewServer(&mcp.Implementation{Name: "ack", Version: "1"}, nil)
			srv.AddTool(&mcp.Tool{Name: "save", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "已保存"}}}
					res.Meta = mcp.Meta{"yanshi/page": tc.value}
					return res, nil
				})
			hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
			defer hs.Close()
			connector := &mcpcap.Connector{Servers: []mcpcap.Server{{Name: "ack", BusinessLine: "bl", URL: hs.URL, Timeout: time.Second, AttestsSpeakers: tc.declared}}}
			defer connector.Close()
			clk := clock.NewFake(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "test", Version: "1", Model: "test/m", Capabilities: []string{"mcp:ack/*"}})
			if err != nil {
				t.Fatal(err)
			}
			gw := model.NewGateway()
			gw.Register("test", toolCallOnce{tool: "mcp_ack__save", args: `{}`})
			core := app.NewCore(app.Memory(clk), app.Options{Clock: clk, IDs: ids.Sequential, Agents: agents, Model: gw, MCP: connector})
			st := &session.State{SessionID: "ses_v1_000_000_s"}
			if err := core.Store.Commit(ctx, st,
				&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "test", Version: "1"}}}},
				&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r", Input: model.TextBlocks("整理")}}}); err != nil {
				t.Fatal(err)
			}
			if err := core.Stores.Queue.Enqueue(ctx, "ses_v1_000_000_s", workqueue.Class{}); err != nil {
				t.Fatal(err)
			}
			w := runtime.New(core.WorkerConfig("w", workqueue.Pool{}, nil))
			for range 12 {
				if _, err := w.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			events, err := eventlog.ReadAll(ctx, core.Store.Log, "ses_v1_000_000_s", 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range events {
				if res := e.GetToolResult(); res != nil {
					found = true
					wantText := "已保存"
					if tc.want {
						wantText = "（已读完：之后没有更多旁听）"
					}
					if res.GetIsError() || res.GetAttested() != tc.want || model.Text(res.GetContent()) != wantText {
						t.Fatalf("result = %v", res)
					}
				}
			}
			projected, err := session.Reduce(events)
			if err != nil {
				t.Fatal(err)
			}
			external := false
			for _, p := range projected.Pieces() {
				if p.From == session.FromExternal {
					external = true
				}
			}
			if external == tc.want {
				t.Fatal("incorrect page provenance")
			}
			if !found {
				t.Fatal("no tool result")
			}
		})
	}
}

// toolCallOnce 先调用一次工具，看到工具结果后结束 Run。
type toolCallOnce struct{ tool, args string }

func (m toolCallOnce) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			return &model.Response{Content: model.TextBlocks("好")}, nil
		}
	}
	return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "c", Capability: m.tool, ArgumentsJson: m.args}}}, nil
}
