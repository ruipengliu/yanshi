// Package xinlimock 是旁听业务线 xinli 的模拟 MCP Server（docs/design/captures.md §6、docs/design/captures-by-exchange.md
// §5–§7）：评测（internal/eval）与开发部署（serve -xinli-mock，调试控制台）共用。数据只在进程内存中。
//
// 业务线的数据按 App 的页面组织：今日（Day：每日整理的总览、要点，与当天交出的交谈）、项目（Project：现状与按天的
// 时间线）、承诺（Commitment：承诺账本，按引用核对承诺人）、待确认（归属拿不准的交谈）、觉察（Day.Awareness）。
// 开发部署中它还扮演业务线服务端，做每日流程的"归集"（Business）。
package xinlimock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yanshi/internal/mcpcap"
	"yanshi/internal/memory"
)

// Server 是模拟的 xinli MCP Server：查询类工具只读；交出结果的工具（file_exchange、file_day、set_status、
// update_commitments）只把结果交给业务线，登记中以 no_approval 放宽。数据按调用方的业务线与 EndUser 隔离，可由 Seed
// 预置（评测用例的 setup.xinli）。
type Server struct {
	// Servers 是交给 mcpcap.Connector 的登记，每个业务线一条；都开启 cite_captures，承诺据此核对承诺人。
	Servers []mcpcap.Server
	// Business 非 nil 时扮演业务线服务端，做每日流程的"归集"：file_day 之后为当天涉及的项目创建（或复用）xinli-project
	// Session，提交"整理 <日期>"。评测中为 nil：用例直接预置 day_summary 的内容。
	Business Business
	// Now 决定交出的结果记在当地的哪一天，默认 time.Now。
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

// Project 是业务线的一个项目。ID、Name、Status 可由评测预置；现状之后由 xinli-project 的 set_status 更新。
type Project struct {
	ID            string    `yaml:"id" json:"id"`
	Name          string    `yaml:"name" json:"name"`
	Status        string    `yaml:"status" json:"status"`
	NextSteps     []string  `yaml:"-" json:"next_steps"`
	OpenQuestions []string  `yaml:"-" json:"open_questions"`
	StatusAt      time.Time `yaml:"-" json:"status_at,omitzero"`
	// Session 是项目的 xinli-project Session（归集时创建）。
	Session string `yaml:"-" json:"session,omitempty"`
	// Archived 为 true 时项目已归档：不再被召回，App 中可以取消归档（captures-weekly-review.md §3）。
	Archived bool `yaml:"-" json:"archived,omitempty"`
	// MergedInto 非空时项目已并入另一个项目（用户确认了合并提议）。
	MergedInto string `yaml:"-" json:"merged_into,omitempty"`
}

// Active 报告项目是否参与召回：没有归档，也没有被合并。
func (p *Project) Active() bool { return !p.Archived && p.MergedInto == "" }

// Exchange 是交出的一段交谈（file_exchange），按（旁听 Session，交谈 ID）去重。
type Exchange struct {
	// ID 是交谈 ID（如 "x57.0"）；Session 是它所在的旁听 Session。
	ID      string    `yaml:"id" json:"id"`
	Session string    `yaml:"-" json:"session"`
	Date    string    `yaml:"date" json:"date"`
	FiledAt time.Time `yaml:"-" json:"filed_at"`
	Title   string    `yaml:"title" json:"title"`
	Summary string    `yaml:"summary" json:"summary"`
	// Match 是归属：existing、new、unsure、none；Project 是归入的项目，unsure 时为空直到用户确认。
	Match     string   `yaml:"match" json:"match"`
	Project   string   `yaml:"project" json:"project,omitempty"`
	Candidate string   `json:"candidate,omitempty"` // unsure：可能属于的项目
	Proposed  string   `json:"proposed,omitempty"`  // unsure：用户不认同候选时新建的项目名
	Decisions []string `yaml:"decisions" json:"decisions"`
	Risks     []string `yaml:"risks" json:"risks"`
	Awareness []string `yaml:"awareness" json:"awareness"`
}

// Pending 报告这段交谈是否在"待确认"中：归属拿不准，用户还没有确认。
func (e *Exchange) Pending() bool { return e.Match == "unsure" && e.Project == "" }

// Commitment 是承诺账本中的一条（captures-by-exchange.md §7）。
type Commitment struct {
	ID          string   `yaml:"id" json:"id"`
	Kind        string   `yaml:"kind" json:"kind"` // mine：佩戴者答应别人；theirs：别人答应佩戴者
	Content     string   `yaml:"content" json:"content"`
	Counterpart string   `yaml:"counterpart" json:"counterpart,omitempty"`
	DueDate     string   `yaml:"due_date" json:"due_date,omitempty"`
	DueText     string   `yaml:"due_text" json:"due_text,omitempty"`
	Project     string   `yaml:"project" json:"project,omitempty"`
	Refs        []string `yaml:"-" json:"refs,omitempty"`
	// Unverified 非空时是没有通过核对的原因：照常展示，但不按期限提醒。
	Unverified string `yaml:"-" json:"unverified,omitempty"`
	// Status 是 open、done 或 cancelled。
	Status    string    `yaml:"-" json:"status"`
	Exchange  string    `yaml:"-" json:"exchange,omitempty"`
	Session   string    `yaml:"-" json:"session,omitempty"`
	UpdatedAt time.Time `yaml:"-" json:"updated_at,omitzero"`
}

// Day 是一次每日整理（file_day）的结果。
type Day struct {
	Date       string    `yaml:"date" json:"date"`
	Session    string    `yaml:"-" json:"session"`
	FiledAt    time.Time `yaml:"-" json:"filed_at"`
	Headline   string    `yaml:"headline" json:"headline"`
	Highlights []string  `yaml:"highlights" json:"highlights"`
	Awareness  Awareness `yaml:"awareness" json:"awareness"`
}

// Awareness 是对用户本人沟通方式的观察（"觉察"页）：不含情绪与身心状态，它们只以 health 类别写入 Memory。
type Awareness struct {
	Habits      []string `yaml:"habits" json:"habits"`
	Suggestions []string `yaml:"suggestions" json:"suggestions"`
}

// State 是一个 EndUser 在业务线中的全部数据，供调试控制台展示（GET /v1/debug/xinli）。
type State struct {
	Projects    []Project    `json:"projects"`
	Exchanges   []Exchange   `json:"exchanges"`
	Commitments []Commitment `json:"commitments"`
	Days        []Day        `json:"days"`
	Weeks       []Week       `json:"weeks"`
	Merges      []Merge      `json:"merges"`
}

type user struct {
	projects    []Project
	exchanges   []Exchange
	commitments []Commitment
	days        []Day
	weeks       []Week
	merges      []Merge
	// weekSessions 是每周回顾的 Session，按本周第一天。
	weekSessions map[string]string
	// seeded 是评测预置的按日期摘要（day_summary、recent_days 在没有整理结果时返回它）。
	seeded map[string]string
	// next 是新建项目的编号：从 101 开始，与用例预置的项目 ID 区分；nextC 是承诺的编号。
	next, nextC int
}

// NewProject 是新建的第一个项目的 ID；评测用例据此断言之后的整理归入了同一项目。
const NewProject = "P-0101"

// call 是一次工具调用：调用方的身份与旁听引用的解析取自 _meta（mcpcap 注入，docs/design/m5-mcp.md）。
type call struct {
	x                              *Server
	u                              *user
	businessLine, endUser, session string
	args                           json.RawMessage
	cites                          map[string]mcpcap.Citation
}

func readOnly() *mcp.ToolAnnotations { return &mcp.ToolAnnotations{ReadOnlyHint: true} }

const noArgs = `{"type":"object","properties":{}}`

var tools = []struct {
	tool    *mcp.Tool
	handler func(c *call) string
}{
	{&mcp.Tool{Name: "list_projects", Description: "列出用户已有的项目（ID、名称与现状）。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()},
		func(c *call) string {
			var b strings.Builder
			for i := range c.u.projects {
				if c.u.projects[i].Active() {
					b.WriteString(c.u.projectLine(&c.u.projects[i]))
				}
			}
			if b.Len() == 0 {
				return "还没有项目"
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "find_projects", Description: "按主题召回至多 5 个可能相关的已有项目（ID、名称、现状、未完成的承诺数），供判断一段交谈归属哪个项目。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"交谈的主题，如\"新办公室装修 报价\""}},"required":["query"]}`),
		Annotations: readOnly()},
		func(c *call) string {
			var in struct{ Query string }
			_ = json.Unmarshal(c.args, &in)
			// 生产中由业务线按嵌入或全文检索；模拟服务用字符二元组相似度，相同时按项目顺序。归档与已合并的不召回。
			var idx []int
			score := make([]float64, len(c.u.projects))
			for i := range c.u.projects {
				p := &c.u.projects[i]
				if p.Active() {
					idx, score[i] = append(idx, i), memory.TextSimilarity(in.Query, p.Name+" "+p.Status)
				}
			}
			if len(idx) == 0 {
				return "还没有项目"
			}
			slices.SortStableFunc(idx, func(a, b int) int {
				switch {
				case score[a] > score[b]:
					return -1
				case score[a] < score[b]:
					return 1
				}
				return 0
			})
			var b strings.Builder
			for _, i := range idx[:min(5, len(idx))] {
				b.WriteString(c.u.projectLine(&c.u.projects[i]))
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "project_digest", Description: "返回一个项目的现状、按天的时间线与未完成的承诺。在项目自己的会话中可以不填 project。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","description":"项目 ID；项目会话中省略即本项目"}}}`),
		Annotations: readOnly()},
		func(c *call) string {
			var in struct{ Project string }
			_ = json.Unmarshal(c.args, &in)
			p := c.u.project(in.Project)
			if in.Project == "" {
				p = c.u.projectOfSession(c.session)
			}
			if p == nil {
				return "没有这个项目"
			}
			var b strings.Builder
			b.WriteString(c.u.projectLine(p))
			list(&b, "下一步", p.NextSteps)
			list(&b, "待解决", p.OpenQuestions)
			for _, e := range c.u.exchanges {
				if e.Project == p.ID {
					fmt.Fprintf(&b, "\n%s %s\n%s", e.Date, e.Title, exchangeText(&e))
				}
			}
			c.u.openCommitments(&b, p.ID)
			return b.String()
		}},
	{&mcp.Tool{Name: "recent_days", Description: "返回最近几天（按日期）的整理摘要与交谈。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()},
		func(c *call) string {
			var b strings.Builder
			for _, d := range c.u.sortedDays() {
				fmt.Fprintf(&b, "%s：%s\n", d.Date, d.Headline)
				for _, h := range d.Highlights {
					fmt.Fprintf(&b, "- %s\n", h)
				}
				for _, e := range c.u.exchanges {
					if e.Session == d.Session {
						fmt.Fprintf(&b, "  [%s] %s：%s\n", c.u.where(&e), e.Title, e.Summary)
					}
				}
			}
			for _, d := range slices.Sorted(maps.Keys(c.u.seeded)) {
				fmt.Fprintf(&b, "%s：%s\n", d, c.u.seeded[d])
			}
			if b.Len() == 0 {
				return "最近没有整理过的内容"
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "file_exchange", Description: "交出一段交谈的整理结果，每段值得记录的交谈调用一次；同一段交谈再次交出时取代之前的结果。" +
		"project 是归属：existing（明确是 find_projects 召回的某个项目，填 id）、new（值得持续跟进的新主题，填 name）、" +
		"unsure（可能是某个候选但拿不准：填候选的 id，再给一个新项目名 name，交给用户确认）、none（一次性的事，不归项目）。" +
		"commitments 是双方的承诺，每条用 refs 引用说出它的原话（旁听中每行开头的编号，如 #57.3）：业务线按被引用原话的说话人核对承诺人。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"exchange":{"type":"string","description":"交谈 ID，取自 captures 页首，如 x57.0"},` +
			`"title":{"type":"string","description":"简短的标题"},` +
			`"summary":{"type":"string","description":"谈了什么、进展到哪；别人提出而用户没有答应的要求写在这里"},` +
			`"project":{"type":"object","properties":{"match":{"type":"string","enum":["existing","new","unsure","none"]},` +
			`"id":{"type":"string","description":"existing 与 unsure：项目 ID"},"name":{"type":"string","description":"new 与 unsure：新项目的名称"}},"required":["match"]},` +
			`"decisions":{"type":"array","items":{"type":"string"},"description":"已经定下的事"},` +
			`"commitments":{"type":"array","items":{"type":"object","properties":{` +
			`"kind":{"type":"string","enum":["mine","theirs"],"description":"mine：用户本人答应别人；theirs：别人答应用户"},` +
			`"content":{"type":"string","description":"要做的事"},"counterpart":{"type":"string","description":"答应了谁，或谁答应的（按原话称呼）"},` +
			`"due_date":{"type":"string","description":"能确定日期时写 YYYY-MM-DD"},"due_text":{"type":"string","description":"原话中的期限说法，如\"明天上午\""},` +
			`"refs":{"type":"array","items":{"type":"string"},"description":"说出这个承诺的原话编号，如 [\"#57.3\"]"}},` +
			`"required":["kind","content","refs"]}},` +
			`"risks":{"type":"array","items":{"type":"string"},"description":"可能出问题的地方：延期、分歧、遗漏"},` +
			`"awareness":{"type":"array","items":{"type":"string"},"description":"这段交谈中用户本人沟通方式的观察，附例子；不写情绪与身心状态"}},` +
			`"required":["exchange","title","summary","project"]}`)},
		(*call).fileExchange},
	{&mcp.Tool{Name: "day_exchanges", Description: "返回今天已经交出的交谈：ID、标题、摘要、归属、承诺数与沟通方式的观察，供写当天总览。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()},
		func(c *call) string {
			var b strings.Builder
			n := 0
			for _, e := range c.u.exchanges {
				if e.Session != c.session {
					continue
				}
				n++
				fmt.Fprintf(&b, "%s %s（%s）：%s\n", e.ID, e.Title, c.u.where(&e), e.Summary)
				list(&b, "  已定", e.Decisions)
				list(&b, "  风险", e.Risks)
				list(&b, "  沟通方式", e.Awareness)
				for _, m := range c.u.commitments {
					if m.Session == e.Session && m.Exchange == e.ID {
						fmt.Fprintf(&b, "  承诺：%s\n", commitmentText(&m))
					}
				}
			}
			if n == 0 {
				return "今天还没有交出交谈"
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "file_day", Description: "交出当天的总览与觉察，读完全部旁听、交出各段交谈之后调用一次。day 是当天的总览；" +
		"awareness 是对用户本人沟通方式的观察与建议。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"day":{"type":"object","description":"当天的总览","properties":{` +
			`"headline":{"type":"string","description":"一句话概括这一天"},` +
			`"highlights":{"type":"array","items":{"type":"string"},"description":"当天最重要的几件事（3 条以内）"}},"required":["headline"]},` +
			`"awareness":{"type":"object","description":"对用户本人沟通方式的观察；不写情绪与身心状态","properties":{` +
			`"habits":{"type":"array","items":{"type":"string"},"description":"沟通习惯，各附当天的一个例子"},` +
			`"suggestions":{"type":"array","items":{"type":"string"},"description":"下次可以怎么做"}}}},` +
			`"required":["day"]}`)},
		(*call).fileDay},
	{&mcp.Tool{Name: "day_summary", Description: "取回某天归入本项目的交谈，以及本项目当前未完成的承诺（带 ID）。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","description":"日期 YYYY-MM-DD"}},"required":["date"]}`),
		Annotations: readOnly()},
		func(c *call) string {
			var in struct{ Date string }
			_ = json.Unmarshal(c.args, &in)
			p := c.u.projectOfSession(c.session)
			var b strings.Builder
			if p != nil {
				for _, e := range c.u.exchanges {
					if e.Project == p.ID && e.Date == in.Date {
						fmt.Fprintf(&b, "%s\n%s", e.Title, exchangeText(&e))
					}
				}
			}
			if b.Len() == 0 {
				if s, ok := c.u.seeded[in.Date]; ok {
					b.WriteString(s + "\n")
				} else {
					b.WriteString("这一天没有与本项目有关的交谈\n")
				}
			}
			if p != nil {
				c.u.openCommitments(&b, p.ID)
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "set_status", Description: "更新本项目的现状（整体替换）：现在进展到哪、下一步、待解决的问题。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","description":"现状，两三句话"},` +
			`"next_steps":{"type":"array","items":{"type":"string"}},"open_questions":{"type":"array","items":{"type":"string"}}},"required":["status"]}`)},
		func(c *call) string {
			var in struct {
				Status        string
				NextSteps     []string `json:"next_steps"`
				OpenQuestions []string `json:"open_questions"`
			}
			_ = json.Unmarshal(c.args, &in)
			p := c.u.projectOfSession(c.session)
			if p == nil {
				return "这个会话不对应任何项目"
			}
			p.Status, p.NextSteps, p.OpenQuestions, p.StatusAt = in.Status, in.NextSteps, in.OpenQuestions, c.x.now()
			return "已更新 " + p.ID + " 的现状"
		}},
	{&mcp.Tool{Name: "update_commitments", Description: "对账本项目的承诺：done 是已完成的、cancelled 是取消的承诺 ID，due_changes 是改了期限的。新的承诺不在这里添加。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"done":{"type":"array","items":{"type":"string"}},` +
			`"cancelled":{"type":"array","items":{"type":"string"}},` +
			`"due_changes":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"due_date":{"type":"string"}},"required":["id","due_date"]}}}}`)},
		(*call).updateCommitments},
}

func (c *call) fileExchange() string {
	var in struct {
		Exchange, Title, Summary string
		Project                  struct{ Match, ID, Name string }
		Decisions, Risks         []string
		Awareness                []string
		Commitments              []struct {
			Kind, Content, Counterpart string
			DueDate                    string `json:"due_date"`
			DueText                    string `json:"due_text"`
			Refs                       []string
		}
	}
	if err := json.Unmarshal(c.args, &in); err != nil || in.Exchange == "" {
		return "参数无效：需要 exchange（captures 页首的交谈 ID）"
	}
	u, now := c.u, c.x.now()
	e := Exchange{ID: in.Exchange, Session: c.session, Date: now.Format(time.DateOnly), FiledAt: now, Title: in.Title, Summary: in.Summary,
		Match: in.Project.Match, Decisions: in.Decisions, Risks: in.Risks, Awareness: in.Awareness}
	switch in.Project.Match {
	case "existing":
		if u.project(in.Project.ID) == nil {
			return fmt.Sprintf("没有项目 %s：用 find_projects 召回的 ID，或改为 new、unsure", in.Project.ID)
		}
		e.Project = in.Project.ID
	case "new":
		name := in.Project.Name
		if name == "" {
			name = in.Title
		}
		// 同一段交谈重交时沿用上次新建的项目，不再新建。
		if old := u.exchange(c.session, in.Exchange); old != nil && old.Match == "new" && old.Project != "" {
			e.Project = old.Project
		} else {
			e.Project = u.newProject(name)
		}
	case "unsure":
		if u.project(in.Project.ID) == nil {
			return fmt.Sprintf("没有项目 %s：unsure 的 id 须是 find_projects 召回的候选", in.Project.ID)
		}
		e.Candidate, e.Proposed = in.Project.ID, in.Project.Name
	case "none":
	default:
		return "参数无效：project.match 须是 existing、new、unsure 或 none"
	}
	// 同一段交谈重交（重试、接管）：取代之前的结果与它带来的承诺。
	u.exchanges = slices.DeleteFunc(u.exchanges, func(x Exchange) bool { return x.Session == c.session && x.ID == in.Exchange })
	u.commitments = slices.DeleteFunc(u.commitments, func(m Commitment) bool { return m.Session == c.session && m.Exchange == in.Exchange })
	u.exchanges = append(u.exchanges, e)
	var b strings.Builder
	fmt.Fprintf(&b, "已收到交谈 %s：%s", e.ID, u.where(&e))
	unverified := 0
	for _, m := range in.Commitments {
		u.nextC++
		cm := Commitment{ID: fmt.Sprintf("C-%04d", u.nextC), Kind: m.Kind, Content: m.Content, Counterpart: m.Counterpart, DueDate: m.DueDate,
			DueText: m.DueText, Project: e.Project, Refs: m.Refs, Status: "open", Exchange: e.ID, Session: c.session, UpdatedAt: now,
			Unverified: c.verify(m.Kind, m.Refs)}
		if cm.Unverified != "" {
			unverified++
		}
		u.commitments = append(u.commitments, cm)
	}
	fmt.Fprintf(&b, "；承诺 %d 条", len(in.Commitments))
	if len(in.Commitments) > 0 && unverified == 0 {
		b.WriteString("，均已核实")
	}
	if unverified > 0 {
		fmt.Fprintf(&b, "，其中 %d 条未核实（不会按期限提醒）：", unverified)
		for _, m := range u.commitments {
			if m.Session == c.session && m.Exchange == e.ID && m.Unverified != "" {
				fmt.Fprintf(&b, "「%s」%s；", m.Content, m.Unverified)
			}
		}
	}
	return b.String()
}

// verify 按平台给出的被引用原话的说话人核对承诺人（captures-by-exchange.md §7）：mine 须引用佩戴者的话，theirs 须引用
// 他人的话。返回未通过的原因，通过时为空。
func (c *call) verify(kind string, refs []string) string {
	want, who := "wearer", "用户本人"
	if kind == "theirs" {
		want, who = "other", "对方"
	}
	found := false
	for _, r := range refs {
		ct, ok := c.cites[r]
		if ok && !ct.Missing {
			found = true
			if ct.Speaker == want {
				return ""
			}
		}
	}
	if !found {
		return "引用的原话不存在"
	}
	return "引用的原话里没有" + who + "说的话"
}

func (c *call) fileDay() string {
	var in struct {
		Day struct {
			Headline   string
			Highlights []string
		}
		Awareness Awareness
	}
	_ = json.Unmarshal(c.args, &in)
	now := c.x.now()
	day := Day{Date: now.Format(time.DateOnly), Session: c.session, FiledAt: now, Headline: in.Day.Headline,
		Highlights: in.Day.Highlights, Awareness: in.Awareness}
	// 同一 Session 再次整理（重试）时取代之前的结果。
	if c.session != "" {
		c.u.days = slices.DeleteFunc(c.u.days, func(d Day) bool { return d.Session == c.session })
	}
	c.u.days = append(c.u.days, day)
	var touched []string
	n := 0
	for _, e := range c.u.exchanges {
		if e.Session == c.session {
			n++
			if e.Project != "" && !slices.Contains(touched, e.Project) {
				touched = append(touched, e.Project)
			}
		}
	}
	if c.x.Business != nil && len(touched) > 0 {
		go c.x.collect(c.businessLine, c.endUser, c.session, day.Date, touched)
	}
	// 总要有确认：没有交谈（当天只有闲聊）时返回空文字，模型会以为没有交上、一再重交。
	return fmt.Sprintf("已收到 %s 的整理（%d 段交谈）", day.Date, n)
}

func (c *call) updateCommitments() string {
	var in struct {
		Done, Cancelled []string
		DueChanges      []struct {
			ID      string
			DueDate string `json:"due_date"`
		} `json:"due_changes"`
	}
	_ = json.Unmarshal(c.args, &in)
	p := c.u.projectOfSession(c.session)
	if p == nil {
		return "这个会话不对应任何项目"
	}
	var b strings.Builder
	var unknown []string
	set := func(id string, f func(*Commitment)) {
		for i := range c.u.commitments {
			if m := &c.u.commitments[i]; m.ID == id && m.Project == p.ID {
				f(m)
				m.UpdatedAt = c.x.now()
				return
			}
		}
		unknown = append(unknown, id)
	}
	for _, id := range in.Done {
		set(id, func(m *Commitment) { m.Status = "done" })
	}
	for _, id := range in.Cancelled {
		set(id, func(m *Commitment) { m.Status = "cancelled" })
	}
	for _, d := range in.DueChanges {
		set(d.ID, func(m *Commitment) { m.DueDate = d.DueDate })
	}
	fmt.Fprintf(&b, "已更新 %d 条承诺", len(in.Done)+len(in.Cancelled)+len(in.DueChanges)-len(unknown))
	if len(unknown) > 0 {
		fmt.Fprintf(&b, "；不是本项目的承诺：%s", strings.Join(unknown, "、"))
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

// Resolve 是用户在 App 的"待确认"中的选择（captures-by-exchange.md §6）：把旁听 Session daySession 中拿不准归属的交谈
// exchange 归入项目 project；project 为空时按交谈给出的名称新建。交谈的承诺随之归入项目。
func (x *Server) Resolve(businessLine, endUser, daySession, exchange, project string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	e := u.exchange(daySession, exchange)
	if e == nil || !e.Pending() {
		return fmt.Errorf("exchange %s of %s is not pending", exchange, daySession)
	}
	if project == "" {
		name := e.Proposed
		if name == "" {
			name = e.Title
		}
		project = u.newProject(name)
	} else if u.project(project) == nil {
		return fmt.Errorf("no project %s", project)
	}
	e.Project = project
	for i := range u.commitments {
		if m := &u.commitments[i]; m.Session == daySession && m.Exchange == exchange {
			m.Project = project
		}
	}
	return nil
}

// Start 在回环地址上启动模拟的 xinli MCP Server，ctx 结束时关闭；businessLines 中的每个业务线各登记一次（同名同地址，
// 共用连接）。
func Start(ctx context.Context, businessLines ...string) (*Server, error) {
	x := &Server{ctx: ctx, users: map[string]*user{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "xinli", Version: "2"}, nil)
	for _, t := range tools {
		srv.AddTool(t.tool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			meta := func(k string) string { s, _ := req.Params.Meta["yanshi/"+k].(string); return s }
			c := &call{x: x, businessLine: meta("business_line"), endUser: meta("end_user"), session: meta("session"), args: req.Params.Arguments,
				cites: citations(req.Params.Meta["yanshi/captures"])}
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
		s := mcpcap.Server{Name: "xinli", BusinessLine: bl, URL: "http://" + ln.Addr().String(), Timeout: 30 * time.Second, CiteCaptures: true}
		s.Tools.NoApproval = []string{"file_exchange", "file_day", "set_status", "update_commitments", "propose_merge", "file_week"}
		x.Servers = append(x.Servers, s)
	}
	return x, nil
}

// citations 解析 _meta["yanshi/captures"]（经 JSON 往返后是 []any）。
func citations(v any) map[string]mcpcap.Citation {
	raw, _ := json.Marshal(v)
	var list []mcpcap.Citation
	_ = json.Unmarshal(raw, &list)
	out := make(map[string]mcpcap.Citation, len(list))
	for _, c := range list {
		out[c.Ref] = c
	}
	return out
}

// SeedData 是预置的数据（评测用例的 setup.xinli）。
type SeedData struct {
	Projects []Project
	// Days 是按日期的摘要：day_summary、recent_days 在没有交出的交谈时返回它。
	Days        map[string]string
	Commitments []Commitment
	// Filed 是已经交出的每日整理（日期、总览与觉察），Exchanges 是已经交出的交谈：每周回顾与重写现状读取它们。
	Filed     []Day
	Exchanges []Exchange
}

// Seed 为业务线中的 endUser 预置数据。
func (x *Server) Seed(businessLine, endUser string, d SeedData) {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	u.projects = append(u.projects, d.Projects...)
	for day, sum := range d.Days {
		u.seeded[day] = sum
	}
	for _, m := range d.Commitments {
		if m.ID == "" {
			u.nextC++
			m.ID = fmt.Sprintf("C-%04d", u.nextC)
		}
		if m.Status == "" {
			m.Status = "open"
		}
		u.commitments = append(u.commitments, m)
	}
	for i, day := range d.Filed {
		day.Session = fmt.Sprintf("seed_day_%d", i)
		u.days = append(u.days, day)
	}
	for i, e := range d.Exchanges {
		if e.ID == "" {
			e.ID = fmt.Sprintf("x%d.0", i+1)
		}
		e.Session = "seed_" + e.Date
		if e.Match == "" && e.Project != "" {
			e.Match = "existing"
		}
		u.exchanges = append(u.exchanges, e)
	}
}

// Bind 记下项目 project 的 xinli-project Session 是 sessionID（评测中用例的 Session 扮演归集时创建的项目 Session）。
func (x *Server) Bind(businessLine, endUser, project, sessionID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if p := x.user(businessLine, endUser).project(project); p != nil {
		p.Session = sessionID
	}
}

// State 返回业务线中 endUser 的数据（拷贝），整理结果按日期从新到旧。
func (x *Server) State(businessLine, endUser string) State {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	st := State{Projects: slices.Clone(u.projects), Exchanges: slices.Clone(u.exchanges), Commitments: slices.Clone(u.commitments), Days: u.sortedDays(),
		Weeks: slices.Clone(u.weeks), Merges: slices.Clone(u.merges)}
	if st.Weeks == nil {
		st.Weeks = []Week{}
	}
	if st.Merges == nil {
		st.Merges = []Merge{}
	}
	slices.Reverse(st.Days)
	if st.Projects == nil {
		st.Projects = []Project{}
	}
	if st.Exchanges == nil {
		st.Exchanges = []Exchange{}
	}
	if st.Commitments == nil {
		st.Commitments = []Commitment{}
	}
	return st
}

// user 返回业务线中 EndUser 的数据；调用方持有 mu。
func (x *Server) user(businessLine, endUser string) *user {
	k := businessLine + "\x00" + endUser
	u := x.users[k]
	if u == nil {
		u = &user{seeded: map[string]string{}, weekSessions: map[string]string{}, next: 101}
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

func (u *user) newProject(name string) string {
	id := fmt.Sprintf("P-%04d", u.next)
	u.next++
	u.projects = append(u.projects, Project{ID: id, Name: name})
	return id
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

func (u *user) exchange(session, id string) *Exchange {
	if i := slices.IndexFunc(u.exchanges, func(e Exchange) bool { return e.Session == session && e.ID == id }); i >= 0 {
		return &u.exchanges[i]
	}
	return nil
}

// where 呈现一段交谈的归属。
func (u *user) where(e *Exchange) string {
	switch {
	case e.Project != "":
		name := ""
		if p := u.project(e.Project); p != nil {
			name = " " + p.Name
		}
		return "归入 " + e.Project + name
	case e.Pending():
		return "待用户确认：可能属于 " + e.Candidate
	}
	return "不归项目"
}

func (u *user) projectLine(p *Project) string {
	open := 0
	for _, m := range u.commitments {
		if m.Project == p.ID && m.Status == "open" {
			open++
		}
	}
	status := p.Status
	if status == "" {
		status = "（还没有现状）"
	}
	return fmt.Sprintf("%s %s：%s（未完成的承诺 %d 条）\n", p.ID, p.Name, status, open)
}

// openCommitments 列出项目未完成的承诺，供 Project 的 Run 对账。
func (u *user) openCommitments(b *strings.Builder, project string) {
	first := true
	for _, m := range u.commitments {
		if m.Project != project || m.Status != "open" {
			continue
		}
		if first {
			b.WriteString("\n未完成的承诺：\n")
			first = false
		}
		fmt.Fprintf(b, "- %s %s\n", m.ID, commitmentText(&m))
	}
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

// exchangeText 把一段交谈呈现为给 Project 的 Run 与助手看的文字。
func exchangeText(e *Exchange) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", e.Summary)
	list(&b, "已定", e.Decisions)
	list(&b, "风险", e.Risks)
	return b.String()
}

func list(b *strings.Builder, label string, items []string) {
	if len(items) > 0 {
		fmt.Fprintf(b, "%s：%s\n", label, strings.Join(items, "；"))
	}
}

func commitmentText(m *Commitment) string {
	s := m.Content
	switch {
	case m.Kind == "theirs" && m.Counterpart != "":
		s = m.Counterpart + "答应用户：" + s
	case m.Kind == "theirs":
		s = "对方答应用户：" + s
	case m.Counterpart != "":
		s = "用户答应" + m.Counterpart + "：" + s
	default:
		s = "用户答应：" + s
	}
	switch {
	case m.DueDate != "" && m.DueText != "":
		s += "，期限 " + m.DueDate + "（" + m.DueText + "）"
	case m.DueDate != "":
		s += "，期限 " + m.DueDate
	case m.DueText != "":
		s += "，期限 " + m.DueText
	}
	if m.Unverified != "" {
		s += "（未核实：" + m.Unverified + "）"
	}
	return s
}
