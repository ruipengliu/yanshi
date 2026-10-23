// Package xinlimock 是旁听业务线 xinli 的模拟 MCP Server（docs/design/captures.md §6）：评测（internal/eval）与
// 开发部署（serve -xinli-mock，调试控制台）共用。数据只在进程内存中。
//
// 业务线的数据按 App 的三个页面组织：今日（Day：每次每日整理的总览、要点）、项目（Project：跨天的时间线与承诺）、
// 觉察（Day.Awareness：沟通习惯与建议）。开发部署中它还扮演业务线服务端，做每日流程的"归集"（Business）。
package xinlimock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/mcpcap"
)

// Server 是模拟的 xinli MCP Server：查询类工具只读；file_day 与 set_commitments 只把结果交给业务线，登记中以 no_approval
// 放宽。数据按调用方的业务线与 EndUser 隔离，可由 Seed 预置（评测用例的 setup.xinli）。
type Server struct {
	// Servers 是交给 mcpcap.Connector 的登记，每个业务线一条。
	Servers []mcpcap.Server
	// Business 非 nil 时扮演业务线服务端，做每日流程的第 4 步"归集"：file_day 之后为涉及的项目创建（或复用）xinli-project
	// Session，提交"整理 <日期>"。评测中为 nil：用例直接预置 day_summary 的内容。
	Business Business
	// Now 决定 file_day 记在当地的哪一天，默认 time.Now。
	Now    func() time.Time
	Logger *slog.Logger

	ctx   context.Context
	mu    sync.Mutex
	users map[string]*user
}

// Business 是扮演业务线服务端时用到的 yanshi 接口（serve 中由 service 提供，以业务线的身份调用）。
type Business interface {
	CreateSession(ctx context.Context, businessLine, endUser, agent string) (string, error)
	// Submit 提交一条文字输入；inputID 相同的重复提交只生效一次。
	Submit(ctx context.Context, sessionID, inputID, text string) error
}

// ProjectAgent 是归集时为项目创建的 Session 的 AgentDef。
const ProjectAgent = "xinli-project"

// Project 是业务线的一个项目。ID、Name、Digest 可由评测预置；其余由整理结果累积。
type Project struct {
	ID     string `yaml:"id" json:"id"`
	Name   string `yaml:"name" json:"name"`
	Digest string `yaml:"digest" json:"digest"`
	// Session 是项目的 xinli-project Session（归集时创建）。
	Session     string       `yaml:"-" json:"session,omitempty"`
	Timeline    []Entry      `yaml:"-" json:"timeline"`
	Commitments []Commitment `yaml:"-" json:"commitments"`
	// CommitmentsAt 是承诺列表最近一次更新的时刻。
	CommitmentsAt time.Time `yaml:"-" json:"commitments_at,omitzero"`
}

// Entry 是项目时间线上的一天：那天的整理中归入本项目的分组。
type Entry struct {
	Date       string `json:"date"`
	DaySession string `json:"day_session"`
	Group
}

// Commitment 是用户本人答应别人要做的事，业务线按期限提醒。
type Commitment struct {
	Content string `json:"content"`
	To      string `json:"to,omitempty"`
	Due     string `json:"due,omitempty"`
}

// Day 是一次每日整理（file_day）的结果。
type Day struct {
	Date       string    `json:"date"`
	Session    string    `json:"session"`
	FiledAt    time.Time `json:"filed_at"`
	Headline   string    `json:"headline"`
	Highlights []string  `json:"highlights"`
	Groups     []Group   `json:"groups"`
	Awareness  Awareness `json:"awareness"`
}

// Group 是一天中归入一个项目的对话。
type Group struct {
	Project    string   `json:"project"`
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Decisions  []string `json:"decisions"`
	MyTodos    []string `json:"my_todos"`
	TheirTodos []string `json:"their_todos"`
	Risks      []string `json:"risks"`
}

// Awareness 是对用户本人沟通方式的观察（"觉察"页）：不含情绪与身心状态，它们只以 health 类别写入 Memory（§7）。
type Awareness struct {
	Habits      []string `json:"habits"`
	Suggestions []string `json:"suggestions"`
}

// State 是一个 EndUser 在业务线中的全部数据，供调试控制台展示（GET /v1/debug/xinli）。
type State struct {
	Projects []Project `json:"projects"`
	Days     []Day     `json:"days"`
}

type user struct {
	projects []Project
	days     []Day
	// seeded 是评测预置的按日期摘要（day_summary、recent_days 在没有整理结果时返回它）。
	seeded map[string]string
	// next 是新建项目的编号：从 101 开始，与用例预置的项目 ID 区分。
	next int
}

// NewProject 是 file_day 新建的第一个项目的 ID；评测用例据此断言之后的整理归入了同一项目。
const NewProject = "P-0101"

// call 是一次工具调用：调用方的身份取自 _meta（mcpcap 注入，docs/design/m5-mcp.md）。
type call struct {
	x                              *Server
	u                              *user
	businessLine, endUser, session string
	args                           json.RawMessage
}

var tools = []struct {
	tool    *mcp.Tool
	handler func(c *call) string
}{
	{&mcp.Tool{Name: "list_projects", Description: "列出用户已有的项目（ID、名称与近况）。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(c *call) string {
			if len(c.u.projects) == 0 {
				return "还没有项目"
			}
			var b strings.Builder
			for _, p := range c.u.projects {
				fmt.Fprintf(&b, "%s %s：%s\n", p.ID, p.Name, p.Digest)
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "project_digest", Description: "返回一个项目的近况、按天的时间线与当前的承诺。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","description":"项目 ID"}},"required":["project"]}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(c *call) string {
			var in struct{ Project string }
			_ = json.Unmarshal(c.args, &in)
			p := c.u.project(in.Project)
			if p == nil {
				return "没有这个项目"
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s %s：%s\n", p.ID, p.Name, p.Digest)
			for _, e := range p.Timeline {
				fmt.Fprintf(&b, "\n%s\n%s", e.Date, groupText(e.Group))
			}
			if len(p.Commitments) > 0 {
				b.WriteString("\n用户本人的承诺：\n")
				for _, m := range p.Commitments {
					fmt.Fprintf(&b, "- %s\n", commitmentText(m))
				}
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "recent_days", Description: "返回最近几天（按日期）的整理摘要。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(c *call) string {
			var b strings.Builder
			for _, d := range c.u.sortedDays() {
				fmt.Fprintf(&b, "%s：%s\n", d.Date, d.Headline)
				for _, h := range d.Highlights {
					fmt.Fprintf(&b, "- %s\n", h)
				}
				for _, g := range d.Groups {
					fmt.Fprintf(&b, "  [%s %s] %s\n", g.Project, g.Title, g.Summary)
				}
			}
			dates := make([]string, 0, len(c.u.seeded))
			for d := range c.u.seeded {
				dates = append(dates, d)
			}
			slices.Sort(dates)
			for _, d := range dates {
				fmt.Fprintf(&b, "%s：%s\n", d, c.u.seeded[d])
			}
			if b.Len() == 0 {
				return "最近没有整理过的内容"
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "day_summary", Description: "取回某天整理出的、与本项目有关的摘要。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","description":"日期 YYYY-MM-DD"}},"required":["date"]}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(c *call) string {
			var in struct{ Date string }
			_ = json.Unmarshal(c.args, &in)
			if p := c.u.projectOfSession(c.session); p != nil {
				var b strings.Builder
				for _, e := range p.Timeline {
					if e.Date == in.Date {
						b.WriteString(groupText(e.Group))
					}
				}
				if b.Len() > 0 {
					return b.String()
				}
			}
			if s, ok := c.u.seeded[in.Date]; ok {
				return s
			}
			return "这一天没有与本项目有关的摘要"
		}},
	{&mcp.Tool{Name: "file_day", Description: "交出当天的整理结果，每天只调用一次。day 是当天的总览；groups 按项目分组：project 为已有项目的 ID，" +
		"不属于任何已有项目时留空（新建项目，title 作为项目名称）；awareness 是对用户本人沟通方式的观察与建议。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"day":{"type":"object","description":"当天的总览","properties":{` +
			`"headline":{"type":"string","description":"一句话概括这一天"},` +
			`"highlights":{"type":"array","items":{"type":"string"},"description":"当天最重要的几件事（3 条以内）"}},"required":["headline"]},` +
			`"groups":{"type":"array","items":{"type":"object","properties":{` +
			`"project":{"type":"string","description":"已有项目的 ID；新主题留空"},"title":{"type":"string","description":"项目名称"},` +
			`"summary":{"type":"string","description":"谈了什么、进展到哪"},` +
			`"decisions":{"type":"array","items":{"type":"string"},"description":"已经定下的事"},` +
			`"my_todos":{"type":"array","items":{"type":"string"},"description":"用户本人答应要做的事，带期限"},` +
			`"their_todos":{"type":"array","items":{"type":"string"},"description":"对方答应要做的事，注明是谁、期限"},` +
			`"risks":{"type":"array","items":{"type":"string"},"description":"可能出问题的地方：延期、分歧、遗漏"}},` +
			`"required":["title","summary"]}},` +
			`"awareness":{"type":"object","description":"对用户本人沟通方式的观察；不写情绪与身心状态","properties":{` +
			`"habits":{"type":"array","items":{"type":"string"},"description":"沟通习惯，各附当天的一个例子"},` +
			`"suggestions":{"type":"array","items":{"type":"string"},"description":"下次可以怎么做"}}}},` +
			`"required":["day","groups"]}`)},
		(*call).fileDay},
	{&mcp.Tool{Name: "set_commitments", Description: "交出本项目当前的承诺列表（整体替换）：用户答应别人要做的事，业务线按期限提醒用户。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"commitments":{"type":"array","items":{"type":"object","properties":{` +
			`"content":{"type":"string","description":"要做的事"},"to":{"type":"string","description":"答应了谁"},` +
			`"due":{"type":"string","description":"期限"}},"required":["content"]}}},"required":["commitments"]}`)},
		func(c *call) string {
			var in struct{ Commitments []Commitment }
			_ = json.Unmarshal(c.args, &in)
			if p := c.u.projectOfSession(c.session); p != nil {
				p.Commitments = in.Commitments
				p.CommitmentsAt = c.x.now()
			}
			return fmt.Sprintf("已更新 %d 条承诺", len(in.Commitments))
		}},
}

func (c *call) fileDay() string {
	var in struct {
		Day struct {
			Headline   string
			Highlights []string
		}
		Groups    []Group
		Awareness Awareness
	}
	_ = json.Unmarshal(c.args, &in)
	now := c.x.now()
	day := Day{Date: now.Format(time.DateOnly), Session: c.session, FiledAt: now, Headline: in.Day.Headline,
		Highlights: in.Day.Highlights, Awareness: in.Awareness}
	// 同一 Session 再次整理（重试）时取代之前的结果：先撤下它在各项目时间线上留下的分组。
	if c.session != "" {
		for i := range c.u.projects {
			p := &c.u.projects[i]
			p.Timeline = slices.DeleteFunc(p.Timeline, func(e Entry) bool { return e.DaySession == c.session })
		}
		c.u.days = slices.DeleteFunc(c.u.days, func(d Day) bool { return d.Session == c.session })
	}
	var b strings.Builder
	var touched []string
	for _, g := range in.Groups {
		p := c.u.project(g.Project)
		if p == nil {
			c.u.projects = append(c.u.projects, Project{ID: fmt.Sprintf("P-%04d", c.u.next), Name: g.Title})
			c.u.next++
			p = &c.u.projects[len(c.u.projects)-1]
			fmt.Fprintf(&b, "已新建项目 %s %s\n", p.ID, p.Name)
		} else {
			fmt.Fprintf(&b, "已归入 %s %s\n", p.ID, p.Name)
		}
		g.Project = p.ID
		p.Digest = g.Summary
		p.Timeline = append(p.Timeline, Entry{Date: day.Date, DaySession: c.session, Group: g})
		day.Groups = append(day.Groups, g)
		touched = append(touched, p.ID)
	}
	c.u.days = append(c.u.days, day)
	// 总要有确认：没有分组（当天只有闲聊）时返回空文字，模型会以为没有交上、一再重交。
	fmt.Fprintf(&b, "已收到 %s 的整理（%d 个分组）", day.Date, len(day.Groups))
	if c.x.Business != nil && len(touched) > 0 {
		go c.x.collect(c.businessLine, c.endUser, c.session, day.Date, touched)
	}
	return b.String()
}

// collect 扮演业务线服务端做"归集"（docs/design/captures.md §6 第 4 步）：为每个涉及的项目创建（或复用）xinli-project
// Session，提交"整理 <日期>"；输入中不含摘要，由 Project 的 Run 经 day_summary 拉取（ADR-0037）。输入 ID 由整理所在的
// Session 与项目派生：重复归集只生效一次。
func (x *Server) collect(businessLine, endUser, daySession, date string, projects []string) {
	for _, id := range projects {
		x.mu.Lock()
		u := x.user(businessLine, endUser)
		p := u.project(id)
		sid := ""
		if p != nil {
			sid = p.Session
		}
		x.mu.Unlock()
		if p == nil {
			continue
		}
		if sid == "" {
			var err error
			if sid, err = x.Business.CreateSession(x.ctx, businessLine, endUser, ProjectAgent); err != nil {
				x.logger().Warn("xinli mock: create project session", "project", id, "err", err)
				continue
			}
			x.mu.Lock()
			if p := u.project(id); p != nil {
				p.Session = sid
			}
			x.mu.Unlock()
		}
		if err := x.Business.Submit(x.ctx, sid, "xinli_"+daySession+"_"+id, "整理 "+date); err != nil {
			x.logger().Warn("xinli mock: submit to project session", "project", id, "session_id", sid, "err", err)
		}
	}
}

// Start 在回环地址上启动模拟的 xinli MCP Server，ctx 结束时关闭；businessLines 中的每个业务线各登记一次（同名同地址，
// 共用连接）。
func Start(ctx context.Context, businessLines ...string) (*Server, error) {
	x := &Server{ctx: ctx, users: map[string]*user{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "xinli", Version: "1"}, nil)
	for _, t := range tools {
		srv.AddTool(t.tool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			meta := func(k string) string { s, _ := req.Params.Meta["yanshi/"+k].(string); return s }
			c := &call{x: x, businessLine: meta("business_line"), endUser: meta("end_user"), session: meta("session"), args: req.Params.Arguments}
			x.mu.Lock()
			c.u = x.user(c.businessLine, c.endUser)
			out := t.handler(c)
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
		x.Servers = append(x.Servers, s)
	}
	return x, nil
}

// Seed 为业务线中的 endUser 预置项目与按日期的摘要（day_summary、recent_days 在没有整理结果时返回它）。
func (x *Server) Seed(businessLine, endUser string, projects []Project, days map[string]string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	u.projects = append(u.projects, projects...)
	for d, sum := range days {
		u.seeded[d] = sum
	}
}

// State 返回业务线中 endUser 的项目与整理结果（拷贝），整理结果按日期从新到旧。
func (x *Server) State(businessLine, endUser string) State {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	st := State{Projects: slices.Clone(u.projects), Days: u.sortedDays()}
	slices.Reverse(st.Days)
	if st.Projects == nil {
		st.Projects = []Project{}
	}
	return st
}

// user 返回业务线中 EndUser 的数据；调用方持有 mu。
func (x *Server) user(businessLine, endUser string) *user {
	k := businessLine + "\x00" + endUser
	u := x.users[k]
	if u == nil {
		u = &user{seeded: map[string]string{}, next: 101}
		x.users[k] = u
	}
	return u
}

func (x *Server) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

func (x *Server) logger() *slog.Logger {
	if x.Logger != nil {
		return x.Logger
	}
	return slog.Default()
}

func (u *user) project(id string) *Project {
	if id == "" {
		return nil
	}
	if i := slices.IndexFunc(u.projects, func(p Project) bool { return p.ID == id }); i >= 0 {
		return &u.projects[i]
	}
	return nil
}

func (u *user) projectOfSession(sid string) *Project {
	if sid == "" {
		return nil
	}
	if i := slices.IndexFunc(u.projects, func(p Project) bool { return p.Session == sid }); i >= 0 {
		return &u.projects[i]
	}
	return nil
}

// sortedDays 返回按日期、整理时刻从旧到新的整理结果（拷贝）。
func (u *user) sortedDays() []Day {
	days := slices.Clone(u.days)
	slices.SortStableFunc(days, func(a, b Day) int {
		if c := strings.Compare(a.Date, b.Date); c != 0 {
			return c
		}
		return a.FiledAt.Compare(b.FiledAt)
	})
	return days
}

// groupText 把一个分组呈现为给 Project 的 Run 看的文字：用户本人答应的与对方答应的分开写明。
func groupText(g Group) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", g.Summary)
	list := func(label string, items []string) {
		if len(items) > 0 {
			fmt.Fprintf(&b, "%s：%s\n", label, strings.Join(items, "；"))
		}
	}
	list("已定", g.Decisions)
	list("用户本人答应", g.MyTodos)
	list("对方答应", g.TheirTodos)
	list("风险", g.Risks)
	return b.String()
}

func commitmentText(m Commitment) string {
	s := m.Content
	if m.To != "" {
		s += "（答应 " + m.To + "）"
	}
	if m.Due != "" {
		s += "，期限 " + m.Due
	}
	return s
}
