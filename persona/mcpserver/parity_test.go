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
	"persona/fixture"
	"persona/store/memstore"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	Op        string          `json:"op"`
	Name      string          `json:"name,omitempty"`
	Session   string          `json:"session,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Citations json.RawMessage `json:"citations,omitempty"`
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
	res, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: step.Name, Arguments: args, Meta: mcp.Meta{
		"yanshi/business_line": script.BusinessLine, "yanshi/end_user": script.EndUser, "yanshi/session": step.Session, "yanshi/captures": cites,
	}})
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
	s := memstore.New()
	now := func() time.Time { return script.Now }
	mux := http.NewServeMux()
	mux.Handle("/mcp", Handler(s, Options{Now: now}))
	mux.Handle("/v1/fixture/", fixture.Handler(s, fixture.Options{Now: now}))
	hs := httptest.NewServer(mux)
	defer hs.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "parity", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	actual := parityGolden{Version: script.Version, Tools: parityTools(t, ctx, conn)}
	if !*updateParity {
		parityEqual(t, "tools/list", actual.Tools, want.Tools)
	}
	for i, step := range script.Steps {
		var got parityOutput
		if step.Op == "tool" {
			got = parityCall(t, ctx, conn, script, step)
		} else {
			got = parityOutput{Input: step}
			method := "POST"
			if step.Op == "state" || step.Op == "week" {
				method = "GET"
			}
			request, err := http.NewRequestWithContext(ctx, method, hs.URL+"/v1/fixture/"+script.BusinessLine+"/"+script.EndUser+"/"+step.Op, bytes.NewReader(step.Args))
			if err != nil {
				t.Fatal(err)
			}
			response, err := hs.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode == http.StatusOK {
				err = json.NewDecoder(response.Body).Decode(&got.Value)
			} else {
				if response.StatusCode != http.StatusBadRequest {
					t.Fatalf("step %d: HTTP %d", i, response.StatusCode)
				}
				var value struct{ Error string }
				err = json.NewDecoder(response.Body).Decode(&value)
				got.Error = value.Error
			}
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
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
