package mcpcap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/capability"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
)

func TestAttestedRegistrationAndBusinessLine(t *testing.T) {
	var seenOptions atomic.Int32
	srv := mcp.NewServer(&mcp.Implementation{Name: "ack", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "save", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if options, ok := req.Params.Meta["yanshi/page_options"]; ok {
				raw, _ := json.Marshal(options)
				seenOptions.Add(1)
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				if fields["limit"] != float64(500) || fields["max_chars"] != float64(6000) || fields["gap_ms"] != float64(300000) {
					t.Error("invalid page options")
				}
			}
			res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "saved"}}}
			res.Meta = mcp.Meta{"yanshi/page": map[string]string{"kind": "empty"}}
			return res, nil
		})
	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer hs.Close()
	for _, declared := range []bool{false, true} {
		conn := &mcpcap.Connector{Servers: []mcpcap.Server{{Name: "ack", BusinessLine: "bl", URL: hs.URL, Timeout: time.Second, AttestsSpeakers: declared}}}
		t.Cleanup(conn.Close)
		tools, err := conn.Tools(context.Background(), "bl", []string{"ack/*"})
		if err != nil || len(tools) != 1 {
			t.Fatalf("tools: %d, %v", len(tools), err)
		}
		for _, bl := range []string{"bl", "other"} {
			res, err := tools[0].Local.(capability.ResultInvoker).InvokeResult(context.Background(), capability.Invocation{BusinessLine: bl, SessionID: "s", Arguments: `{}`})
			if err != nil {
				t.Fatal(err)
			}
			if (len(res.Page) > 0) != (declared && bl == "bl") || (model.Text(res.Content) == "saved") == (declared && bl == "bl") {
				t.Fatalf("declared %v, business line %s: %+v", declared, bl, res)
			}
		}
		foreign, err := conn.Tools(context.Background(), "other", []string{"ack/*"})
		if err != nil || len(foreign) != 0 {
			t.Fatalf("foreign tools = %v, %v", foreign, err)
		}
	}
	if seenOptions.Load() != 2 {
		t.Fatal("missing page_options")
	}
}

func TestLoadAttestedRegistration(t *testing.T) {
	for _, value := range []string{"", "true", "false", "invalid"} {
		dir := t.TempDir()
		body := "name: ack\nbusiness_line: bl\nurl: https://example.com/mcp\n"
		if value != "" {
			body += "attests_speakers: " + value + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "ack.yaml"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		servers, err := mcpcap.LoadDir(dir)
		if value == "invalid" {
			if err == nil {
				t.Fatal("invalid attests_speakers accepted")
			}
			continue
		}
		if err != nil || len(servers) != 1 || servers[0].AttestsSpeakers != (value == "true") {
			t.Fatalf("acks %s: %+v, %v", value, servers, err)
		}
	}
}
