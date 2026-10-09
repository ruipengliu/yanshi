package mcpcap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/mcpcap"
	"yanshi/internal/node"
)

// crm 是一个远程 MCP Server：记录收到的请求头与 _meta，提供不同注解的工具。
type crm struct {
	mu      sync.Mutex
	headers http.Header
	meta    mcp.Meta
}

func (c *crm) start(t *testing.T) string {
	t.Helper()
	truth := true
	srv := mcp.NewServer(&mcp.Implementation{Name: "crm", Version: "1"}, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	srv.AddTool(&mcp.Tool{Name: "search_customer", Description: "查客户", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			c.mu.Lock()
			c.meta = req.Params.Meta
			c.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "张三，VIP"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "create_ticket", InputSchema: schema, Annotations: &mcp.ToolAnnotations{IdempotentHint: true}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "工单 T-1"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "delete_all", InputSchema: schema, Annotations: &mcp.ToolAnnotations{DestructiveHint: &truth}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "refused"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "chart", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "见图"}, &mcp.ImageContent{MIMEType: "image/png", Data: []byte("PNG")}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "slow", InputSchema: schema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "late"}}}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		if r.Header.Get("X-Yanshi-End-User") != "" {
			c.headers = r.Header.Clone()
		}
		c.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	return hs.URL
}

func setup(t *testing.T) (*crm, *capability.Catalog, *artifact.Service) {
	c := &crm{}
	url := c.start(t)
	s := mcpcap.Server{Name: "crm", BusinessLine: "demo", URL: url, Timeout: 500 * time.Millisecond,
		Headers: map[string]string{"Authorization": "Bearer secret"}}
	s.Tools.Allow = []string{"search_*", "create_ticket", "delete_all", "chart", "slow"}
	s.Tools.NoApproval = []string{"create_ticket", "delete_all"}
	down := mcpcap.Server{Name: "down", BusinessLine: "demo", URL: "http://127.0.0.1:1/mcp", Timeout: 200 * time.Millisecond}
	other := mcpcap.Server{Name: "erp", BusinessLine: "other", URL: url, Timeout: time.Second}
	arts := &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: artifact.NewMemBlobs(), IDs: ids.Random(), Clock: clock.Real{}}
	conn := &mcpcap.Connector{Servers: []mcpcap.Server{s, down, other}, Artifacts: arts}
	t.Cleanup(conn.Close)
	return c, &capability.Catalog{Local: capability.NewRegistry(), MCP: conn}, arts
}

var target = capability.Target{Scope: node.Scope{BusinessLine: "demo", EndUser: "u1"}, SessionID: "s1"}

func tools(t *testing.T, cat *capability.Catalog, allow ...string) map[string]capability.Tool {
	t.Helper()
	list, err := cat.Tools(context.Background(), target, allow)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]capability.Tool{}
	for _, x := range list {
		out[x.Spec.Name] = x
	}
	return out
}

func invoke(t *testing.T, tool capability.Tool, args string) ([]*v1.ContentBlock, error) {
	t.Helper()
	return tool.Local.Invoke(context.Background(), capability.Invocation{SessionID: "s1", RunID: "r", CallID: "c",
		Arguments: args, BusinessLine: "demo", EndUser: "u1"})
}

func TestDiscoveryScopingAndRisk(t *testing.T) {
	_, cat, _ := setup(t)
	all := tools(t, cat, "mcp:*/*")
	if _, ok := all["mcp_erp__search_customer"]; ok {
		t.Fatal("another business line's MCP server is visible")
	}
	cases := map[string]struct {
		approval, idempotent bool
	}{
		"mcp_crm__search_customer": {false, true}, // 只读
		"mcp_crm__create_ticket":   {false, true}, // 业务线放宽
		"mcp_crm__delete_all":      {true, false}, // 明确的破坏性工具不能放宽
	}
	for name, want := range cases {
		x, ok := all[name]
		if !ok {
			t.Fatalf("%s missing (have %v)", name, all)
		}
		if x.RequiresApproval() != want.approval || x.Spec.Idempotent != want.idempotent {
			t.Errorf("%s: approval %v idempotent %v, want %+v", name, x.RequiresApproval(), x.Spec.Idempotent, want)
		}
	}
	if only := tools(t, cat, "mcp:crm/search_*"); len(only) != 1 {
		t.Fatalf("glob filter: %v", only)
	}
	if _, err := cat.Tools(context.Background(), target, []string{"mcp:crm"}); err == nil {
		t.Fatal("accepted a pattern without a tool glob")
	}
}

func TestCallPassesIdentityAndMapsResults(t *testing.T) {
	c, cat, arts := setup(t)
	all := tools(t, cat, "mcp:crm/*")
	out, err := invoke(t, all["mcp_crm__search_customer"], `{"q":"张三"}`)
	if err != nil || out[0].GetText().GetText() != "张三，VIP" {
		t.Fatalf("search = %v, %v", out, err)
	}
	c.mu.Lock()
	h, meta := c.headers, c.meta
	c.mu.Unlock()
	if h.Get("Authorization") != "Bearer secret" || h.Get("X-Yanshi-End-User") != "u1" || h.Get("X-Yanshi-Session") != "s1" {
		t.Fatalf("headers = %v", h)
	}
	if meta["yanshi/end_user"] != "u1" || meta["yanshi/business_line"] != "demo" {
		t.Fatalf("_meta = %v", meta)
	}

	out, err = invoke(t, all["mcp_crm__chart"], `{}`)
	if err != nil || len(out) != 2 || !strings.HasPrefix(out[1].GetMedia().GetUri(), "artifact://") {
		t.Fatalf("chart = %v, %v", out, err)
	}
	if list, _ := arts.List(context.Background(), "s1"); len(list) != 1 || list[0].MimeType != "image/png" {
		t.Fatalf("image not stored as an artifact of the session: %v", list)
	}

	if _, err := invoke(t, all["mcp_crm__delete_all"], `{}`); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("tool error not surfaced: %v", err)
	}
	start := time.Now()
	if _, err := invoke(t, all["mcp_crm__slow"], `{}`); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
	}
	// 超时之后连接仍可用。
	if out, err := invoke(t, all["mcp_crm__search_customer"], `{}`); err != nil || len(out) == 0 {
		t.Fatalf("after timeout: %v", err)
	}
}

func TestLoadDirExpandsEnvAndValidates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRM_TOKEN", "tok")
	write := func(name, body string) {
		if err := writeFile(dir+"/"+name, body); err != nil {
			t.Fatal(err)
		}
	}
	write("crm.yaml", "name: crm\nbusiness_line: demo\nurl: https://crm.example.com/mcp\nheaders:\n  Authorization: \"Bearer ${CRM_TOKEN}\"\n")
	servers, err := mcpcap.LoadDir(dir)
	if err != nil || len(servers) != 1 || servers[0].Headers["Authorization"] != "Bearer tok" || servers[0].Timeout != 30*time.Second {
		t.Fatalf("servers = %+v, %v", servers, err)
	}
	write("bad.yaml", "name: bad\nbusiness_line: demo\nurl: https://x/mcp\nheaders:\n  K: \"${NOT_SET_ANYWHERE}\"\n")
	if _, err := mcpcap.LoadDir(dir); err == nil || !strings.Contains(err.Error(), "NOT_SET_ANYWHERE") {
		t.Fatalf("missing env var accepted: %v", err)
	}
	if s, err := mcpcap.LoadDir(t.TempDir() + "/missing"); err != nil || len(s) != 0 {
		t.Fatalf("missing dir: %v, %v", s, err)
	}
}
