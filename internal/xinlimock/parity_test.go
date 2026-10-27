package xinlimock

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// parityScript 的格式在两侧保持一致；通过 JSON 文件共享，不形成跨模块 import。
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
		t.Fatalf("read %s: %v; generate golden with go test ./internal/xinlimock -run '^TestParity$' -update", path, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatal(err)
	}
}

// TestParity 是迁移期的对拍基准生成器；任务④删除 xinlimock 时随包删除。
// 只读对方 testdata 不引入 Persona 依赖；-update 只能在旧实现侧生成基准。
func TestParity(t *testing.T) {
	const directory = "../../persona/mcpserver/testdata/"
	var script parityScript
	readParity(t, directory+"parity_script.json", &script)
	if script.Version != 1 {
		t.Fatal("unsupported script version")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x, err := Start(ctx, script.BusinessLine)
	if err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	x.Now = func() time.Time { return script.Now }
	x.mu.Unlock()
	client := mcp.NewClient(&mcp.Implementation{Name: "parity", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: x.Servers[0].URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got := parityGolden{Version: 1, Tools: parityTools(t, ctx, conn)}
	for _, step := range script.Steps {
		if step.Op == "tool" {
			got.Steps = append(got.Steps, parityCall(t, ctx, conn, script, step))
			continue
		}
		out := parityOutput{Input: step, Value: map[string]any{}}
		var in struct {
			Project, Session, Input, ID, Status, Exchange, From string
			DueDate                                             string `json:"due_date"`
			Accept                                              bool
		}
		if len(step.Args) > 0 && step.Op != "seed" {
			if err := json.Unmarshal(step.Args, &in); err != nil {
				t.Fatal(err)
			}
		}
		bl, eu := script.BusinessLine, script.EndUser
		var err error
		switch step.Op {
		case "seed":
			var seed SeedData
			if err := json.Unmarshal(step.Args, &seed); err != nil {
				t.Fatal(err)
			}
			x.Seed(bl, eu, seed)
		case "state":
			out.Value = x.State(bl, eu)
		case "bind":
			x.Bind(bl, eu, in.Project, in.Session)
		case "submitted":
			x.Submitted(bl, eu, in.Session, in.Input)
		case "edit":
			err = x.Edit(bl, eu, in.ID, in.Status, in.DueDate)
		case "resolve":
			err = x.Resolve(bl, eu, in.Session, in.Exchange, in.Project)
		case "resolve_merge":
			err = x.ResolveMerge(bl, eu, in.ID, in.Accept)
		case "archive":
			ids := x.Archive(bl, eu)
			if ids == nil {
				ids = []string{}
			}
			out.Value = ids
		case "bind_week":
			x.mu.Lock()
			x.user(bl, eu).weekSessions[in.From] = in.Session
			x.mu.Unlock()
		case "week":
			x.mu.Lock()
			u := x.user(bl, eu)
			from := script.Now.AddDate(0, 0, -weekDays+1).Format(time.DateOnly)
			rewrite := []Project{}
			for _, p := range u.projects {
				if p.Active() && p.Session != "" && u.lastActive(p.ID) >= from {
					rewrite = append(rewrite, p)
				}
			}
			out.Value = map[string]any{"from": from, "session": u.weekSessions[from], "rewrite": rewrite}
			x.mu.Unlock()
		default:
			t.Fatalf("unknown operation %s", step.Op)
		}
		if err != nil {
			out.Error = err.Error()
			out.Value = nil
		}
		got.Steps = append(got.Steps, out)
	}
	if *updateParity {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(directory+"parity.json", append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	var want parityGolden
	readParity(t, directory+"parity.json", &want)
	parityEqual(t, "tools/list", got.Tools, want.Tools)
	if got.Version != want.Version || len(got.Steps) != len(want.Steps) {
		t.Fatal("golden version or step count changed")
	}
	for i := range got.Steps {
		parityEqual(t, fmt.Sprintf("step %d %s", i, script.Steps[i].Op), got.Steps[i], want.Steps[i])
	}
}

var updateParity = flag.Bool("update", false, "更新 Persona 迁移对拍基准")
