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

// 从远程响应到日志的完整路径：格式或登记不合格的元数据不影响给模型的成功结果。
func TestMCPAckWrittenByWorker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared bool
		value    any
		want     bool
	}{
		{"accepted", true, map[string]string{"stream": "transcript", "from": "57.0", "through": "60.2"}, true},
		{"undeclared", false, map[string]string{"stream": "transcript", "from": "57.0", "through": "60.2"}, false},
		{"malformed", true, map[string]string{"stream": "transcript", "from": "#57.0", "through": "60.2"}, false},
		{"unknown stream", true, map[string]string{"stream": "photos", "from": "57.0", "through": "60.2"}, false},
		{"reversed", true, map[string]string{"stream": "transcript", "from": "60.2", "through": "57.0"}, false},
		{"wrong type", true, "accepted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv := mcp.NewServer(&mcp.Implementation{Name: "ack", Version: "1"}, nil)
			srv.AddTool(&mcp.Tool{Name: "save", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "已保存"}}}
					res.Meta = mcp.Meta{"yanshi/ack": tc.value}
					return res, nil
				})
			hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
			defer hs.Close()
			connector := &mcpcap.Connector{Servers: []mcpcap.Server{{Name: "ack", BusinessLine: "bl", URL: hs.URL, Timeout: time.Second, Acks: tc.declared}}}
			defer connector.Close()
			clk := clock.NewFake(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "test", Version: "1", Model: "test/m", Capabilities: []string{"mcp:ack/*"}})
			if err != nil {
				t.Fatal(err)
			}
			gw := model.NewGateway()
			gw.Register("test", ackCallOnce{tool: "mcp_ack__save", args: `{}`})
			core := app.NewCore(app.Memory(clk), app.Options{Clock: clk, IDs: ids.Sequential, Agents: agents, Model: gw, MCP: connector})
			st := &session.State{SessionID: "s"}
			if err := core.Store.Commit(ctx, st,
				&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "test", Version: "1"}}}},
				&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r", Input: model.TextBlocks("整理")}}}); err != nil {
				t.Fatal(err)
			}
			if err := core.Stores.Queue.Enqueue(ctx, "s", workqueue.Class{}); err != nil {
				t.Fatal(err)
			}
			w := runtime.New(core.WorkerConfig("w", workqueue.Pool{}, nil))
			for range 12 {
				if _, err := w.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			events, err := eventlog.ReadAll(ctx, core.Store.Log, "s", 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range events {
				if res := e.GetToolResult(); res != nil {
					found = true
					if res.GetIsError() || (res.GetCaptureAck() != nil) != tc.want || model.Text(res.GetContent()) != "已保存" {
						t.Fatalf("result = %v", res)
					}
				}
			}
			if !found {
				t.Fatal("no tool result")
			}
		})
	}
}

// 只发起一次调用，之后结束 Run，避免依赖 Worker 的内部步骤。
type ackCallOnce struct{ tool, args string }

func (m ackCallOnce) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			return &model.Response{Content: model.TextBlocks("好")}, nil
		}
	}
	return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "c", Capability: m.tool, ArgumentsJson: m.args}}}, nil
}
