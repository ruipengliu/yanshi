package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"persona/fixture"
	"persona/store"
	"persona/store/memstore"
)

var updateParity = flag.Bool("update", false, "重写冻结的 Persona 回归 golden")

// parityScript 经 JSON 固定操作序列，防止夹具契约与 MCP 行为在重构中漂移。
type parityScript struct {
	Version      int          `json:"version"`
	Now          time.Time    `json:"now"`
	BusinessLine string       `json:"business_line"`
	EndUser      string       `json:"end_user"`
	Steps        []parityStep `json:"steps"`
}
type parityStep struct {
	Op          string          `json:"op"`
	Name        string          `json:"name,omitempty"`
	Session     string          `json:"session,omitempty"`
	Args        json.RawMessage `json:"args,omitempty"`
	Citations   json.RawMessage `json:"citations,omitempty"`
	PageOptions json.RawMessage `json:"page_options,omitempty"`
}
type parityOutput struct {
	Input   parityStep `json:"input"`
	Text    string     `json:"text,omitempty"`
	Meta    mcp.Meta   `json:"meta,omitempty"`
	IsError bool       `json:"is_error,omitempty"`
	Error   string     `json:"error,omitempty"`
	Value   any        `json:"value,omitempty"`
}
type parityGolden struct {
	Version int            `json:"version"`
	Tools   []parityTool   `json:"tools"`
	Steps   []parityOutput `json:"steps"`
}
type parityTool struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	InputSchema any                  `json:"input_schema"`
	Annotations *mcp.ToolAnnotations `json:"annotations"`
}

func parityTools(t *testing.T, ctx context.Context, conn *mcp.ClientSession) []parityTool {
	t.Helper()
	var out []parityTool
	for tool, err := range conn.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, parityTool{tool.Name, tool.Description, tool.InputSchema, tool.Annotations})
	}
	slices.SortFunc(out, func(a, b parityTool) int { return strings.Compare(a.Name, b.Name) })
	return out
}
func parityCall(t *testing.T, ctx context.Context, conn *mcp.ClientSession, script parityScript, step parityStep) parityOutput {
	t.Helper()
	out := parityOutput{Input: step}
	var args any
	if err := json.Unmarshal(step.Args, &args); err != nil {
		t.Fatal(err)
	}
	var cites any
	if len(step.Citations) > 0 {
		if err := json.Unmarshal(step.Citations, &cites); err != nil {
			t.Fatal(err)
		}
	}
	metadata := mcp.Meta{
		"yanshi/business_line": script.BusinessLine, "yanshi/end_user": script.EndUser, "yanshi/session": step.Session, "yanshi/captures": cites,
	}
	if len(step.PageOptions) > 0 {
		var options any
		if err := json.Unmarshal(step.PageOptions, &options); err != nil {
			t.Fatal(err)
		}
		metadata["yanshi/page_options"] = options
	}
	res, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: step.Name, Arguments: args, Meta: metadata})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if len(res.Content) != 1 {
		t.Fatalf("unexpected content: %+v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("non-text result: %+v", res.Content)
	}
	out.Text, out.Meta, out.IsError = text.Text, res.Meta, res.IsError
	return out
}
func parityEqual(t *testing.T, label string, got, want any) {
	t.Helper()
	normalize := func(value any) any {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var result any
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if !reflect.DeepEqual(normalize(got), normalize(want)) {
		actual, _ := json.MarshalIndent(got, "", "  ")
		expected, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("%s differs\ngot: %s\nwant: %s", label, actual, expected)
	}
}
func readParity(t *testing.T, path string, dst any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v; regenerate in persona with go test ./mcpserver -run '^TestParity$' -update", path, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatal(err)
	}
}

// TestParity 的冻结 golden 最初由现已删除的 xinlimock 生成，P0 迁移时两份实现逐字一致。
// 有意改变行为时用 -update 重写；工具经 Streamable HTTP，操作与 State 经夹具 HTTP。
func TestParity(t *testing.T) {
	var script parityScript
	readParity(t, "testdata/parity_script.json", &script)
	var want parityGolden
	if !*updateParity {
		readParity(t, "testdata/parity.json", &want)
	}
	if script.Version != 1 || (!*updateParity && (want.Version != 1 || len(script.Steps) != len(want.Steps))) {
		t.Fatal("golden version or step count changed")
	}
	ctx := context.Background()
	endpoint := newParityEndpoint(t, memstore.New(captureClock), script.Now)
	defer endpoint.close()
	conn := endpoint.conn
	actual := parityGolden{Version: script.Version, Tools: parityTools(t, ctx, conn)}
	if !*updateParity {
		parityEqual(t, "tools/list", actual.Tools, want.Tools)
	}
	for i, step := range script.Steps {
		got := endpoint.step(t, script, step)
		actual.Steps = append(actual.Steps, got)
		if !*updateParity {
			parityEqual(t, fmt.Sprintf("step %d %s", i, step.Op), got, want.Steps[i])
		}
	}
	if *updateParity {
		raw, err := json.MarshalIndent(actual, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("testdata/parity.json", append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// MCP 与夹具必须使用同一个 Store；重启时同时重建 HTTP 服务与客户端会话。
type parityEndpoint struct {
	hs   *httptest.Server
	conn *mcp.ClientSession
}

func newParityEndpoint(t *testing.T, s store.Store, at time.Time) *parityEndpoint {
	t.Helper()
	now := func() time.Time { return at }
	mux := http.NewServeMux()
	mux.Handle("/mcp", Handler(s, Options{AllowUnmanaged: true, Now: now}))
	mux.Handle("/v1/fixture/", fixture.Handler(s, fixture.Options{Now: now}))
	hs := httptest.NewServer(mux)
	client := mcp.NewClient(&mcp.Implementation{Name: "parity", Version: "1"}, nil)
	conn, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		hs.Close()
		t.Fatal(err)
	}
	return &parityEndpoint{hs: hs, conn: conn}
}

func (e *parityEndpoint) close() {
	_ = e.conn.Close()
	e.hs.Close()
}

func (e *parityEndpoint) step(t *testing.T, script parityScript, step parityStep) parityOutput {
	t.Helper()
	ctx := context.Background()
	if step.Op == "tool" {
		return parityCall(t, ctx, e.conn, script, step)
	}
	got := parityOutput{Input: step}
	method := "POST"
	if step.Op == "state" || step.Op == "week" || step.Op == "deletion_tasks" {
		method = "GET"
	}
	request, err := http.NewRequestWithContext(ctx, method, e.hs.URL+"/v1/fixture/"+script.BusinessLine+"/"+script.EndUser+"/"+step.Op, bytes.NewReader(step.Args))
	if err != nil {
		t.Fatal(err)
	}
	response, err := e.hs.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		err = json.NewDecoder(response.Body).Decode(&got.Value)
	} else {
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: HTTP %d", step.Op, response.StatusCode)
		}
		var value struct{ Error string }
		err = json.NewDecoder(response.Body).Decode(&value)
		got.Error = value.Error
	}
	if err != nil {
		t.Fatal(err)
	}
	return got
}
