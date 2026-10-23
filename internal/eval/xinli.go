package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/mcpcap"
)

// xinliServer 是评测内置的旁听业务线 MCP Server（docs/design/captures.md §6）：查询类工具只读；file_day 与
// set_commitments 只把结果交给业务线，登记中以 no_approval 放宽。数据按调用方的 EndUser 隔离，由用例的 setup.xinli 预置。
type xinliServer struct {
	servers []mcpcap.Server
	mu      sync.Mutex
	users   map[string]*xinliUser
}

type xinliUser struct {
	projects []XinliProject
	days     map[string]string
	// next 是新建项目的编号：从 101 开始，与用例预置的项目 ID 区分。
	next int
}

// xinliNewProject 是 file_day 新建的第一个项目的 ID；用例据此断言之后的整理归入了同一项目。
const xinliNewProject = "P-0101"

var xinliTools = []struct {
	tool    *mcp.Tool
	handler func(x *xinliServer, u *xinliUser, args json.RawMessage) string
}{
	{&mcp.Tool{Name: "list_projects", Description: "列出用户已有的项目（ID、名称与近况）。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ *xinliServer, u *xinliUser, _ json.RawMessage) string {
			if len(u.projects) == 0 {
				return "还没有项目"
			}
			var b strings.Builder
			for _, p := range u.projects {
				fmt.Fprintf(&b, "%s %s：%s\n", p.ID, p.Name, p.Digest)
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "project_digest", Description: "返回一个项目的近况与各天的摘要。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","description":"项目 ID"}},"required":["project"]}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ *xinliServer, u *xinliUser, args json.RawMessage) string {
			var in struct{ Project string }
			_ = json.Unmarshal(args, &in)
			for _, p := range u.projects {
				if p.ID == in.Project {
					return fmt.Sprintf("%s %s：%s", p.ID, p.Name, p.Digest)
				}
			}
			return "没有这个项目"
		}},
	{&mcp.Tool{Name: "recent_days", Description: "返回最近几天（按日期）的整理摘要。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ *xinliServer, u *xinliUser, _ json.RawMessage) string {
			dates := make([]string, 0, len(u.days))
			for d := range u.days {
				dates = append(dates, d)
			}
			slices.Sort(dates)
			if len(dates) == 0 {
				return "最近没有整理过的内容"
			}
			var b strings.Builder
			for _, d := range dates {
				fmt.Fprintf(&b, "%s：%s\n", d, u.days[d])
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "day_summary", Description: "取回某天整理出的、与本项目有关的摘要。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","description":"日期 YYYY-MM-DD"}},"required":["date"]}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ *xinliServer, u *xinliUser, args json.RawMessage) string {
			var in struct{ Date string }
			_ = json.Unmarshal(args, &in)
			if s, ok := u.days[in.Date]; ok {
				return s
			}
			return "这一天没有与本项目有关的摘要"
		}},
	{&mcp.Tool{Name: "file_day", Description: "交出当天的整理结果：按项目分组的摘要与对用户的洞察。groups 中 project 为已有项目的 ID，" +
		"不属于任何已有项目时留空（新建项目，title 作为项目名称）。每天只调用一次。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"groups":{"type":"array","items":{"type":"object","properties":{` +
			`"project":{"type":"string"},"title":{"type":"string"},"summary":{"type":"string"}},"required":["title","summary"]}},` +
			`"insights":{"type":"string"}},"required":["groups"]}`)},
		func(x *xinliServer, u *xinliUser, args json.RawMessage) string {
			var in struct {
				Groups []struct{ Project, Title, Summary string }
			}
			_ = json.Unmarshal(args, &in)
			var b strings.Builder
			for _, g := range in.Groups {
				id := g.Project
				if i := slices.IndexFunc(u.projects, func(p XinliProject) bool { return p.ID == id }); i >= 0 {
					u.projects[i].Digest = g.Summary
					fmt.Fprintf(&b, "已归入 %s %s\n", id, u.projects[i].Name)
					continue
				}
				id = fmt.Sprintf("P-%04d", u.next)
				u.next++
				u.projects = append(u.projects, XinliProject{ID: id, Name: g.Title, Digest: g.Summary})
				fmt.Fprintf(&b, "已新建项目 %s %s\n", id, g.Title)
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "set_commitments", Description: "交出本项目当前的承诺列表（整体替换）：用户答应别人要做的事，业务线按期限提醒用户。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"commitments":{"type":"array","items":{"type":"object","properties":{` +
			`"content":{"type":"string","description":"要做的事"},"to":{"type":"string","description":"答应了谁"},` +
			`"due":{"type":"string","description":"期限"}},"required":["content"]}}},"required":["commitments"]}`)},
		func(_ *xinliServer, _ *xinliUser, args json.RawMessage) string {
			var in struct{ Commitments []json.RawMessage }
			_ = json.Unmarshal(args, &in)
			return fmt.Sprintf("已更新 %d 条承诺", len(in.Commitments))
		}},
}

// startXinli 启动 xinli MCP Server，并为 businessLines 中的每个业务线各登记一次（同名同地址，共用连接）。
func startXinli(ctx context.Context, businessLines ...string) (*xinliServer, error) {
	x := &xinliServer{users: map[string]*xinliUser{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "xinli", Version: "1"}, nil)
	for _, t := range xinliTools {
		srv.AddTool(t.tool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			eu, _ := req.Params.Meta["yanshi/end_user"].(string)
			x.mu.Lock()
			out := t.handler(x, x.user(eu), req.Params.Arguments)
			x.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil
		})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	hs := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	go func() { <-ctx.Done(); _ = hs.Close() }()
	for _, bl := range businessLines {
		s := mcpcap.Server{Name: "xinli", BusinessLine: bl, URL: "http://" + ln.Addr().String(), Timeout: 30 * time.Second}
		s.Tools.NoApproval = []string{"file_day", "set_commitments"}
		x.servers = append(x.servers, s)
	}
	return x, nil
}

// user 返回 EndUser 的数据；调用方持有 mu。
func (x *xinliServer) user(endUser string) *xinliUser {
	u := x.users[endUser]
	if u == nil {
		u = &xinliUser{days: map[string]string{}, next: 101}
		x.users[endUser] = u
	}
	return u
}

// seed 为本次运行的 EndUser 预置项目与每日摘要。
func (x *xinliServer) seed(endUser string, s *XinliSetup) {
	if x == nil || s == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(endUser)
	u.projects = append(u.projects, s.Projects...)
	for d, sum := range s.Days {
		u.days[d] = sum
	}
}
