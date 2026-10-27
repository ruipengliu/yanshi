// Package xinlimock 是旁听业务线 xinli 的模拟 MCP Server（docs/design/captures-xinli.md §4–§7、§9）：评测
// （internal/eval）与开发部署（serve -xinli-mock，调试控制台）共用。数据只在进程内存中。
//
// 业务线的数据按 App 的页面组织：今日（Day：每日整理的总览、要点，与当天的交谈）、项目（Project：现状与按天的
// 时间线）、承诺（Commitment：承诺账本，按引用核对承诺人，带修订历史）、待确认（归属拿不准的交谈）、觉察
// （Day.Awareness）。交谈与承诺以变更集为日志、按 EndUser 叠加得出（ledger.go，docs/design/captures-xinli-model.md）。
// 开发部署中它还扮演业务线服务端，做每日流程的"归集"（Business）。
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
	"yanshi/internal/memory"
)

// Server 是模拟的 xinli MCP Server：查询类工具只读；交出结果的工具（file_changes、file_day、set_status、
// update_commitments 等）只把结果交给业务线，登记中以 no_approval 放宽。数据按调用方的业务线与 EndUser 隔离，可由 Seed
// 预置（评测用例的 setup.xinli）。
type Server struct {
	// Servers 是交给 mcpcap.Connector 的登记，每个业务线一条；都开启 cite_captures，承诺据此核对承诺人与换算期限。
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
	// Archived 为 true 时项目已归档：不再被召回，App 中可以取消归档（captures-xinli.md §7）。
	Archived bool `yaml:"-" json:"archived,omitempty"`
	// MergedInto 非空时项目已并入另一个项目（用户确认了合并提议）：交谈与承诺叠加时换成保留的那个。
	MergedInto string `yaml:"-" json:"merged_into,omitempty"`
}

// Active 报告项目是否参与召回：没有归档，也没有被合并。
func (p *Project) Active() bool { return !p.Archived && p.MergedInto == "" }

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

// State 是一个 EndUser 在业务线中的全部数据，供调试控制台展示（GET /v1/debug/xinli）与评测断言。
type State struct {
	Projects    []Project    `json:"projects"`
	Exchanges   []Exchange   `json:"exchanges"`
	Commitments []Commitment `json:"commitments"`
	Changesets  []Changeset  `json:"changesets"`
	Days        []Day        `json:"days"`
	Weeks       []Week       `json:"weeks"`
	Merges      []Merge      `json:"merges"`
}

type user struct {
	projects []Project
	// changesets 是变更集的日志，ledger 是由它叠加出的交谈与承诺（每次改动之后重新叠加）。
	changesets []Changeset
	ledger
	days   []Day
	weeks  []Week
	merges []Merge
	// weekSessions 是每周回顾的 Session，按本周第一天。
	weekSessions map[string]string
	// submitted 是业务线向每个旁听 Session 最近提交的整理输入（"整理交谈"或"整理今天"）：业务线据此只接受"整理今天"
	// 那次 Run 的 file_day（F7）。
	submitted map[string]string
	// next 是新建项目的编号：从 101 开始，与用例预置的项目 ID 区分；nextC 是承诺的编号，nextCS 是变更集的编号。
	next, nextC, nextCS int
}

// NewProject 是新建的第一个项目的 ID；评测用例据此断言之后的整理归入了同一项目。
const NewProject = "P-0101"

const (
	// relatedWindow 是召回的范围：最后一次修订在这么久之内的未完成承诺（captures-xinli-model.md §12）。
	relatedWindow = 30 * 24 * time.Hour
	// maxRelated 是 find_related 一次至多返回的承诺数。
	maxRelated = 10
	// similarContent 是"内容相近"的阈值（字符二元组的 Dice 系数）：当天整理的重复候选。
	similarContent = 0.3
	// relatedTopic 是 find_related 只凭主题（对方不同）召回一条承诺的相似度下限。
	relatedTopic = 0.15
)

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

// 给模型看的参数说明（file_changes、update_commitments 共用的部分）。
const (
	refsSchema       = `{"type":"array","items":{"type":"string"},"description":"依据的原话编号（旁听每行开头的编号），如 [\"#57.3\"]"}`
	revSchema        = `{"type":"integer","description":"find_related、day_ledger 或 day_summary 给出的 rev，原样照抄"}`
	projectsSchema   = `{"type":"array","description":"这段交谈涉及的项目，可以多个","items":{"type":"object","properties":{"match":{"type":"string","enum":["existing","new","unsure","none"]},"id":{"type":"string","description":"existing 与 unsure：项目 ID"},"name":{"type":"string","description":"new 与 unsure：新项目的名称"}},"required":["match"]}}`
	reviseItemSchema = `{"type":"object","properties":{"id":{"type":"string","description":"承诺 ID，如 C-0012"},"rev":` + revSchema + `,` +
		`"due_date":{"type":"string","description":"新的期限 YYYY-MM-DD"},"due_text":{"type":"string","description":"原话中新的期限说法"},` +
		`"content":{"type":"string"},"counterpart":{"type":"string"},"project":{"type":"string","description":"改正归属时写项目 ID"},`
	closeItemSchema = `{"type":"object","properties":{"id":{"type":"string"},"rev":` + revSchema + `,"status":{"type":"string","enum":["done","cancelled"]},`
)

var fileChangesSchema = `{"type":"object","properties":{` +
	`"source":{"type":"object","description":"这次依据的旁听范围：本次读到的第一段与最后一段的编号；\"整理今天\"的当天整理写 {\"day\": true}",` +
	`"properties":{"from":{"type":"string","description":"如 #57.0"},"to":{"type":"string","description":"如 #80.2"},"day":{"type":"boolean"}}},` +
	`"exchanges":{"type":"object","properties":{` +
	`"create":{"type":"array","description":"这次划出的交谈：一段谈话一项，寒暄闲聊不交","items":{"type":"object","properties":{` +
	`"from":{"type":"string","description":"这段交谈第一段的编号"},"to":{"type":"string","description":"最后一段的编号"},` +
	`"title":{"type":"string"},"summary":{"type":"string","description":"谈了什么、进展到哪；别人提出而用户没有答应的要求写在这里"},` +
	`"projects":` + projectsSchema + `,` +
	`"decisions":{"type":"array","description":"定下的事","items":{"type":"object","properties":{"text":{"type":"string"},"project":{"type":"string","description":"交谈涉及多个项目时写这件事属于哪个（项目 ID 或新项目名）"}},"required":["text"]}},` +
	`"risks":{"type":"array","items":{"type":"string"},"description":"可能延期、有分歧或被遗漏的地方"},` +
	`"awareness":{"type":"array","items":{"type":"string"},"description":"用户本人沟通方式的观察，附例子；不写情绪与身心状态"}},` +
	`"required":["from","to","title","summary","projects"]}},` +
	`"merge":{"type":"array","description":"只在当天整理中：把被停顿切开的同一段谈话并入 into","items":{"type":"object","properties":{` +
	`"into":{"type":"string","description":"保留的交谈 ID"},"from":{"type":"array","items":{"type":"string"}},"title":{"type":"string"},"summary":{"type":"string"},"projects":` + projectsSchema + `},"required":["into","from"]}},` +
	`"revise":{"type":"array","description":"只在当天整理中：改正交谈的标题、摘要或归属","items":{"type":"object","properties":{` +
	`"id":{"type":"string"},"title":{"type":"string"},"summary":{"type":"string"},"projects":` + projectsSchema + `},"required":["id"]}}}},` +
	`"commitments":{"type":"object","properties":{` +
	`"create":{"type":"array","description":"新的承诺","items":{"type":"object","properties":{` +
	`"id":{"type":"string","description":"可省略；同一个 source 重交时，只填上次回执中的 C- 开头的 ID（C- 加数字），明确沿用；其他形式都按临时 ID 忽略"},` +
	`"exchange":{"type":"string","description":"出处：交谈 ID，即 x 加上它的 from 编号，如 x57.0"},` +
	`"kind":{"type":"string","enum":["mine","theirs"],"description":"mine：用户本人答应别人；theirs：别人答应用户"},` +
	`"content":{"type":"string","description":"要做的事"},"counterpart":{"type":"string","description":"答应了谁，或谁答应的（按原话称呼）"},` +
	`"due_date":{"type":"string","description":"能确定日期时写 YYYY-MM-DD"},"due_text":{"type":"string","description":"原话中的期限说法，如\"明天上午\""},` +
	`"project":{"type":"string","description":"缺省随交谈；交谈涉及多个项目时必填（项目 ID 或新项目名）"},` +
	`"refs":` + refsSchema + `},"required":["exchange","kind","content","refs"]}},` +
	`"revise":{"type":"array","description":"改动已有的承诺（改期、改内容、换对方）：依据这次读到的原话","items":` + reviseItemSchema +
	`"refs":` + refsSchema + `,"reason":{"type":"string"}},"required":["id","rev","refs"]}},` +
	`"close":{"type":"array","description":"已完成（done）或不做了、改由别人做（cancelled）的承诺","items":` + closeItemSchema +
	`"refs":` + refsSchema + `,"reason":{"type":"string"}},"required":["id","rev","status","refs"]}},` +
	`"merge":{"type":"array","description":"只在当天整理中：重复的承诺并入 into","items":{"type":"object","properties":{` +
	`"into":{"type":"string"},"from":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}},"required":["into","from"]}}}}},` +
	`"required":["source"]}`

var updateCommitmentsSchema = `{"type":"object","properties":{` +
	`"date":{"type":"string","description":"对账的那一天 YYYY-MM-DD，与 day_summary 相同"},` +
	`"revise":{"type":"array","description":"改了期限或内容的承诺","items":` + reviseItemSchema +
	`"exchanges":{"type":"array","items":{"type":"string"},"description":"依据的交谈 ID（day_summary 给出）"},"reason":{"type":"string"}},"required":["id","rev","exchanges"]}},` +
	`"close":{"type":"array","description":"已完成（done）或取消（cancelled）的承诺","items":` + closeItemSchema +
	`"exchanges":{"type":"array","items":{"type":"string"},"description":"依据的交谈 ID"},"reason":{"type":"string"}},"required":["id","rev","status","exchanges"]}}},` +
	`"required":["date"]}`

var tools = []struct {
	tool    *mcp.Tool
	handler func(c *call) string
}{
	{&mcp.Tool{Name: "list_commitments", Description: "按责任方、状态、期限、对方、主题与项目查询承诺，默认未完成；每页至多 30 条，按期限升序，无期限排后。被合并的不列出。", InputSchema: json.RawMessage(listCommitmentsSchema), Annotations: readOnly()}, (*call).listCommitments},
	{&mcp.Tool{Name: "commitment_history", Description: "查询一条承诺的当前值与修订历史（改前到改后、时间、依据、引用），以及合并关系。", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"承诺 ID"}},"required":["id"]}`), Annotations: readOnly()}, (*call).commitmentHistory},
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
	{&mcp.Tool{Name: "find_related", Description: "为一段交谈召回相关的数据：至多 5 个候选项目（ID、名称、现状），以及与这段的对方或主题相关的" +
		"未完成承诺（不限哪天，带 ID 与 rev），供判断归属，以及这段里的原话是否改动了已有的承诺。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"交谈的主题，如\"新办公室装修 报价\""},` +
			`"counterparts":{"type":"array","items":{"type":"string"},"description":"交谈的对方，按原话称呼，如 [\"张总\"]"}},"required":["query"]}`),
		Annotations: readOnly()},
		(*call).findRelated},
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
				if e.Live() && slices.Contains(e.InProjects(), p.ID) {
					fmt.Fprintf(&b, "\n%s %s %s\n%s", e.Date, e.ID, e.Title, exchangeText(&e, p.ID))
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
					if e.Session == d.Session && e.Live() {
						fmt.Fprintf(&b, "  [%s] %s：%s\n", c.u.where(&e), e.Title, e.Summary)
					}
				}
			}
			for _, e := range c.u.exchanges {
				if e.Session == seedDays {
					fmt.Fprintf(&b, "%s：%s\n", e.Date, e.Summary)
				}
			}
			if b.Len() == 0 {
				return "最近没有整理过的内容"
			}
			return b.String()
		}},
	{&mcp.Tool{Name: "file_changes", Description: "交出一次整理的变更集：这次读到的旁听里划出的交谈、新的承诺，以及这些原话对已有承诺的改动" +
		"（改期、改内容、完成、作废），一次调用交出全部。每项都用编号引用依据的原话，业务线按被引用原话的说话人核对承诺人、按说话那天核对期限；" +
		"整体检查，有一项不通过就整体不收，结果写明原因。同一个 source 再交一次时整体取代上一次。" +
		"projects 是归属：existing（明确是 find_related 召回的某个项目，填 id）、new（值得持续跟进的新主题，填 name）、" +
		"unsure（可能是某个候选但拿不准：填候选的 id，再给一个新项目名 name，交给用户确认）、none（一次性的事，不归项目）。" +
		"白天的整理只能依据本次读到的原话改动已有的承诺；合并重复的承诺与交谈、改正交谈的归属只在当天整理（source 为 {\"day\": true}）中做。",
		InputSchema: json.RawMessage(fileChangesSchema)},
		(*call).fileChanges},
	{&mcp.Tool{Name: "day_ledger", Description: "返回今天的账本，供当天整理与写当天总览：今天的交谈（ID、范围、标题、摘要、归属、沟通方式的观察）、" +
		"今天的承诺与之前未完成而与今天相关的承诺（带 ID、rev、引用与修订），以及按规则找出的可能重复的承诺、可能被切开的交谈。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()},
		(*call).dayLedger},
	{&mcp.Tool{Name: "file_day", Description: "交出当天的总览与觉察，在\"整理今天\"中读完全部旁听、交出变更集之后调用一次。day 是当天的总览；" +
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
	{&mcp.Tool{Name: "day_summary", Description: "取回某天归入本项目的交谈（带交谈 ID），以及本项目当前未完成的承诺（带 ID 与 rev）。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","description":"日期 YYYY-MM-DD"}},"required":["date"]}`),
		Annotations: readOnly()},
		func(c *call) string {
			var in struct{ Date string }
			_ = json.Unmarshal(c.args, &in)
			p := c.u.projectOfSession(c.session)
			var b strings.Builder
			if p != nil {
				for _, e := range c.u.exchanges {
					if e.Live() && e.Date == in.Date && slices.Contains(e.InProjects(), p.ID) {
						fmt.Fprintf(&b, "交谈 %s %s\n%s", e.ID, e.Title, exchangeText(&e, p.ID))
					}
				}
			}
			if b.Len() == 0 {
				for _, e := range c.u.exchanges {
					if e.Session == seedDays && e.Date == in.Date {
						fmt.Fprintf(&b, "交谈 %s\n%s\n", e.ID, e.Summary)
					}
				}
			}
			if b.Len() == 0 {
				b.WriteString("这一天没有与本项目有关的交谈\n")
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
	{&mcp.Tool{Name: "update_commitments", Description: "依据交谈对账本项目的承诺：close 是已完成（done）或取消（cancelled）的，revise 是改了期限或内容的；" +
		"每项带 day_summary 给出的 rev，并用 exchanges 写明依据的交谈 ID。账本在你看到之后又被改过（rev 不符）时整体不收，结果附上现在的状态。" +
		"同一天再交一次时整体取代上一次。新的承诺不在这里添加。",
		InputSchema: json.RawMessage(updateCommitmentsSchema)},
		(*call).updateCommitments},
}

// verify 按平台给出的被引用原话的说话人核对承诺人（captures-xinli.md §6）：mine 须引用佩戴者的话，theirs 须引用
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

// findRelated 是 find_related：候选项目按主题召回（生产中用嵌入或全文检索，模拟服务用字符二元组相似度，相同时按
// 项目顺序；归档与已合并的不召回），相关的承诺按对方与主题召回（captures-xinli-model.md §6、§12）。
func (c *call) findRelated() string {
	var in struct {
		Query        string
		Counterparts strs
	}
	_ = json.Unmarshal(c.args, &in)
	u := c.u
	var b strings.Builder
	var idx []int
	score := make([]float64, len(u.projects))
	for i := range u.projects {
		if p := &u.projects[i]; p.Active() {
			idx, score[i] = append(idx, i), memory.TextSimilarity(in.Query, p.Name+" "+p.Status)
		}
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmpDesc(score[a], score[b]) })
	b.WriteString("候选项目：\n")
	if len(idx) == 0 {
		b.WriteString("（还没有项目）\n")
	}
	for _, i := range idx[:min(5, len(idx))] {
		b.WriteString(u.projectLine(&u.projects[i]))
	}
	// 对方相同的都召回；只是主题相近的要相似到一定程度：字符二元组几乎总有一点重合，不设下限就会把账本整个搬进上下文。
	related := u.related(c.x.now(), func(m *Commitment) float64 {
		s := memory.TextSimilarity(in.Query, m.Content+" "+m.Counterpart)
		if counterpartIn(m.Counterpart, in.Counterparts) {
			return s + 1
		}
		if s < relatedTopic {
			return 0
		}
		return s
	}, maxRelated)
	if len(related) == 0 {
		b.WriteString("相关的未完成承诺：没有\n")
		return b.String()
	}
	b.WriteString("相关的未完成承诺（这段里的原话改动了其中某条时，在 file_changes 中 revise 或 close 它，不要新建）：\n")
	for _, m := range related {
		fmt.Fprintf(&b, "- %s\n", u.commitmentLine(m))
	}
	return b.String()
}

// related 返回最后一次修订在召回范围内的未完成承诺中得分为正的，按得分从高到低，至多 n 条。
func (u *user) related(now time.Time, score func(*Commitment) float64, n int) []*Commitment {
	var out []*Commitment
	scores := map[string]float64{}
	for i := range u.commitments {
		m := &u.commitments[i]
		if !m.Open() || now.Sub(m.UpdatedAt) > relatedWindow {
			continue
		}
		if s := score(m); s > 0 {
			scores[m.ID] = s
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b *Commitment) int { return cmpDesc(scores[a.ID], scores[b.ID]) })
	return out[:min(n, len(out))]
}

func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

// counterpartIn 报告称呼 name 是否与 names 中的某一个相同或互相包含（"黄总"与"黄总监"）。
func counterpartIn(name string, names []string) bool {
	if name == "" {
		return false
	}
	return slices.ContainsFunc(names, func(n string) bool { return n != "" && (strings.Contains(n, name) || strings.Contains(name, n)) })
}

// dayLedger 是 day_ledger：今天（本旁听 Session）的交谈与承诺，之前未完成、与今天的对方或项目相关的承诺，以及确定性的
// 提示：同一对方、内容相近的承诺对，相邻且归属相同的交谈（captures-xinli-model.md §6）。
func (c *call) dayLedger() string {
	u := c.u
	var b strings.Builder
	var today []Exchange
	for _, e := range u.exchanges {
		if e.Session == c.session && e.Live() {
			today = append(today, e)
		}
	}
	slices.SortStableFunc(today, func(a, b Exchange) int {
		switch {
		case refBefore(a.From, b.From):
			return -1
		case refBefore(b.From, a.From):
			return 1
		}
		return 0
	})
	b.WriteString("今天的交谈：\n")
	if len(today) == 0 {
		b.WriteString("（还没有）\n")
	}
	projects, counterparts := map[string]bool{}, []string{}
	for _, e := range today {
		fmt.Fprintf(&b, "%s（%s–%s）%s［%s］：%s\n", e.ID, e.From, e.To, e.Title, u.where(&e), e.Summary)
		var ds []string
		for _, d := range e.Decisions {
			ds = append(ds, d.Text)
		}
		list(&b, "  已定", ds)
		list(&b, "  风险", e.Risks)
		list(&b, "  沟通方式", e.Awareness)
		for _, p := range e.InProjects() {
			projects[p] = true
		}
	}
	var mine []*Commitment
	for i := range u.commitments {
		m := &u.commitments[i]
		if m.Session == c.session || slices.ContainsFunc(m.Revisions, func(r Revision) bool { return u.changesetSession(r.Changeset) == c.session }) {
			mine = append(mine, m)
			if m.Counterpart != "" {
				counterparts = append(counterparts, m.Counterpart)
			}
			if m.Project != "" {
				projects[m.Project] = true
			}
		}
	}
	b.WriteString("今天的承诺：\n")
	if len(mine) == 0 {
		b.WriteString("（还没有）\n")
	}
	for _, m := range mine {
		fmt.Fprintf(&b, "- %s\n", u.commitmentLine(m))
	}
	earlier := u.related(c.x.now(), func(m *Commitment) float64 {
		if slices.Contains(mine, m) {
			return 0
		}
		if counterpartIn(m.Counterpart, counterparts) || (m.Project != "" && projects[m.Project]) {
			return 1
		}
		return 0
	}, len(u.commitments))
	if len(earlier) > 0 {
		b.WriteString("之前未完成、与今天的对方或项目相关的承诺：\n")
		for _, m := range earlier {
			fmt.Fprintf(&b, "- %s\n", u.commitmentLine(m))
		}
	}
	var hints []string
	open := slices.DeleteFunc(append(slices.Clone(mine), earlier...), func(m *Commitment) bool { return !m.Open() })
	for i, a := range open {
		for _, d := range open[i+1:] {
			if possibleDuplicate(a, d) {
				hints = append(hints, fmt.Sprintf("可能重复的承诺：%s 与 %s（同一对方「%s」、内容相近）", a.ID, d.ID, a.Counterpart))
			}
		}
	}
	for i := 1; i < len(today); i++ {
		a, d := today[i-1].InProjects(), today[i].InProjects()
		if len(a) > 0 && slices.Equal(a, d) {
			hints = append(hints, fmt.Sprintf("相邻且归属相同的交谈：%s 与 %s（都归入 %s）", today[i-1].ID, today[i].ID, strings.Join(a, "、")))
		}
	}
	if len(hints) > 0 {
		b.WriteString("提示（按规则找出的候选，判断之后再决定；拿不准就不动作）：\n")
		for _, h := range hints {
			fmt.Fprintf(&b, "- %s\n", h)
		}
	}
	return b.String()
}

func (c *call) fileDay() string {
	if strings.HasPrefix(c.u.submitted[c.session], "整理交谈") {
		return "没有收下：白天的\"整理交谈\"不交当天总览，当天总览只在\"整理今天\"中交出"
	}
	var in struct {
		Day struct {
			Headline   string
			Highlights []string
		}
		Awareness Awareness
	}
	_ = json.Unmarshal(c.args, &in)
	if ws := healthIn(append(append([]string{in.Day.Headline}, in.Day.Highlights...), append(in.Awareness.Habits, in.Awareness.Suggestions...)...)...); len(ws) > 0 {
		return fmt.Sprintf("没有收下：当天总览与觉察含健康信息（「%s」）。这里只写事务与沟通方式，删去关于身心健康的整句（包括他人的），不要换个说法，再交一次", strings.Join(ws, "」「"))
	}
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
		if e.Session == c.session && e.Live() {
			n++
			for _, p := range e.InProjects() {
				if !slices.Contains(touched, p) {
					touched = append(touched, p)
				}
			}
		}
	}
	if c.x.Business != nil && len(touched) > 0 {
		go c.x.collect(c.businessLine, c.endUser, c.session, day.Date, touched)
	}
	// 总要有确认：没有交谈（当天只有闲聊）时返回空文字，模型会以为没有交上、一再重交。
	return fmt.Sprintf("已收到 %s 的整理（%d 段交谈）", day.Date, n)
}

// collect 扮演业务线服务端做"归集"（docs/design/captures-xinli.md §2 第 4 步）：为每个涉及的项目创建（或复用）xinli-project
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

// Submitted 记下业务线向旁听 Session 提交了哪种整理输入（"整理交谈"或"整理今天"）：业务线清楚自己提交的是什么，只接受
// "整理今天"那次 Run 的 file_day（captures-xinli-model.md §6，F7）。没有记下时不限制（调试控制台中随手提交的输入）。
func (x *Server) Submitted(businessLine, endUser, session, input string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.user(businessLine, endUser).submitted[session] = input
}

// Resolve 是用户在 App 的"待确认"中的选择（captures-xinli.md §5）：把旁听 Session daySession 中拿不准归属的交谈
// exchange 归入项目 project；project 为空时按交谈给出的名称新建。交谈中还没有归属的承诺随之归入项目。它记为用户的变更集。
func (x *Server) Resolve(businessLine, endUser, daySession, exchange, project string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	e := u.exchange(daySession, exchange)
	if e == nil || !e.Pending() {
		return fmt.Errorf("exchange %s of %s is not pending", exchange, daySession)
	}
	links := slices.Clone(e.Projects)
	i := slices.IndexFunc(links, func(l Link) bool { return l.Match == "unsure" && l.Project == "" })
	if project == "" {
		name := links[i].Proposed
		if name == "" {
			name = e.Title
		}
		project = u.newProject(name)
	} else if u.project(project) == nil {
		return fmt.Errorf("no project %s", project)
	}
	links[i].Project = project
	cs := Changeset{Session: daySession, Key: fmt.Sprintf("user:%d", u.nextCS+1), Author: AuthorUser, RecordedAt: x.now(),
		ReviseExchanges: []ExchangeRevise{{ID: exchange, Projects: links}}}
	for _, m := range u.commitments {
		if m.Session == daySession && m.Exchange == exchange && m.Project == "" {
			cs.ReviseCommitments = append(cs.ReviseCommitments, CommitmentChange{ID: m.ID, Rev: m.Rev(), Fields: map[string]string{"project": project}})
		}
	}
	u.commit(&cs, -1, nil)
	return nil
}

// Edit 是用户在 App 中改一条承诺：status 非空时标为完成或取消，否则把期限改为 dueDate。它记为用户的变更集，改过的
// 字段之后以用户的为准，模型不再改动（captures-xinli-model.md §5"用户优先"）。
func (x *Server) Edit(businessLine, endUser, id, status, dueDate string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	m := u.commitment(id)
	if m == nil {
		return fmt.Errorf("no commitment %s", id)
	}
	cs := Changeset{Key: fmt.Sprintf("user:%d", u.nextCS+1), Author: AuthorUser, RecordedAt: x.now()}
	switch {
	case status == "done" || status == "cancelled":
		cs.CloseCommitments = []CommitmentChange{{ID: id, Rev: m.Rev(), Status: status}}
	case status != "":
		return fmt.Errorf("status must be done or cancelled")
	case dueDate != "":
		if _, err := time.Parse(time.DateOnly, dueDate); err != nil {
			return fmt.Errorf("due_date must be YYYY-MM-DD")
		}
		cs.ReviseCommitments = []CommitmentChange{{ID: id, Rev: m.Rev(), Fields: map[string]string{"due_date": dueDate, "due_text": ""}}}
	default:
		return fmt.Errorf("nothing to change")
	}
	u.commit(&cs, -1, nil)
	return nil
}

// Start 在回环地址上启动模拟的 xinli MCP Server，ctx 结束时关闭；businessLines 中的每个业务线各登记一次（同名同地址，
// 共用连接）。
func Start(ctx context.Context, businessLines ...string) (*Server, error) {
	x := &Server{ctx: ctx, users: map[string]*user{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "xinli", Version: "3"}, nil)
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
		s.Tools.NoApproval = []string{"file_changes", "file_day", "set_status", "update_commitments", "propose_merge", "file_week"}
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
	// Days 是按日期的摘要：day_summary、recent_days 在没有交出的交谈时返回它，它也可以作为对账的依据（交谈 ID 为
	// "x" 加日期，如 x2026-10-24）。
	Days        map[string]string
	Commitments []SeedCommitment
	// Filed 是已经交出的每日整理（日期、总览与觉察），Exchanges 是已经交出的交谈：每周回顾与重写现状读取它们。
	Filed     []Day
	Exchanges []SeedExchange
}

// SeedCommitment 是预置的承诺。
type SeedCommitment struct {
	ID          string `yaml:"id"`
	Kind        string `yaml:"kind"`
	Content     string `yaml:"content"`
	Counterpart string `yaml:"counterpart"`
	DueDate     string `yaml:"due_date"`
	DueText     string `yaml:"due_text"`
	Project     string `yaml:"project"`
	// Status 缺省为 open。
	Status     string    `yaml:"status"`
	SpokenAt   time.Time `yaml:"spoken_at"`
	Refs       []string  `yaml:"refs"`
	Unverified string    `yaml:"unverified"`
	// Revisions 是新建之后的修订，按给定顺序写为变更集；初始字段仍取上面的值。
	Revisions []SeedRevision `yaml:"revisions"`
}

// SeedRevision 让评测使用与真实写入相同的变更集重放路径，避免直接伪造当前值与历史。
type SeedRevision struct {
	Author     string            `yaml:"author"`
	Session    string            `yaml:"session"`
	Op         string            `yaml:"op"`
	Fields     map[string]string `yaml:"fields"`
	Refs       []string          `yaml:"refs"`
	Exchanges  []string          `yaml:"exchanges"`
	Reason     string            `yaml:"reason"`
	SpokenAt   time.Time         `yaml:"spoken_at"`
	RecordedAt time.Time         `yaml:"recorded_at"`
}

// SeedExchange 是预置的交谈（归入一个项目）。
type SeedExchange struct {
	ID        string   `yaml:"id"`
	Date      string   `yaml:"date"`
	Title     string   `yaml:"title"`
	Summary   string   `yaml:"summary"`
	Project   string   `yaml:"project"`
	Decisions []string `yaml:"decisions"`
	Risks     []string `yaml:"risks"`
}

// seedDays 是预置的按日期摘要所在的（虚构的）Session。
const seedDays = "seed_days"

// Seed 为业务线中的 endUser 预置数据：交谈与承诺记为一个预置的变更集。
func (x *Server) Seed(businessLine, endUser string, d SeedData) {
	x.mu.Lock()
	defer x.mu.Unlock()
	u := x.user(businessLine, endUser)
	u.projects = append(u.projects, d.Projects...)
	cs := Changeset{Key: fmt.Sprintf("seed:%d", u.nextCS+1), Author: AuthorSeed, RecordedAt: x.now()}
	for _, date := range sortedKeys(d.Days) {
		cs.CreateExchanges = append(cs.CreateExchanges, Exchange{ID: "x" + date, Session: seedDays, Date: date, Title: "当天摘要", Summary: d.Days[date],
			Projects: []Link{{Match: "none"}}})
	}
	for i, e := range d.Exchanges {
		if e.ID == "" {
			e.ID = fmt.Sprintf("x%d.0", i+1)
		}
		link := Link{Match: "none"}
		if e.Project != "" {
			link = Link{Match: "existing", Project: e.Project}
		}
		ex := Exchange{ID: e.ID, Session: "seed_" + e.Date, Date: e.Date, Title: e.Title, Summary: e.Summary, Projects: []Link{link}, Risks: e.Risks}
		for _, t := range e.Decisions {
			ex.Decisions = append(ex.Decisions, Decision{Text: t})
		}
		cs.CreateExchanges = append(cs.CreateExchanges, ex)
	}
	d.Commitments = slices.Clone(d.Commitments)
	for i := range d.Commitments {
		m := &d.Commitments[i]
		if m.ID == "" {
			u.nextC++
			m.ID = fmt.Sprintf("C-%04d", u.nextC)
		}
		cs.CreateCommitments = append(cs.CreateCommitments, Commitment{ID: m.ID, Kind: m.Kind, Content: m.Content, Counterpart: m.Counterpart,
			DueDate: m.DueDate, DueText: m.DueText, Project: m.Project, Status: m.Status, SpokenAt: m.SpokenAt, Refs: m.Refs, Unverified: m.Unverified})
	}
	if len(cs.CreateExchanges) > 0 || len(cs.CreateCommitments) > 0 {
		u.commit(&cs, -1, nil)
	}
	for _, m := range d.Commitments {
		for _, r := range m.Revisions {
			at := r.RecordedAt
			if at.IsZero() {
				at = x.now()
			}
			author := r.Author
			if author == "" {
				author = AuthorSeed
			}
			next := Changeset{Key: fmt.Sprintf("seed:%d", u.nextCS+1), Author: author, Session: r.Session, RecordedAt: at}
			change := CommitmentChange{ID: m.ID, Fields: r.Fields, Refs: r.Refs, Exchanges: r.Exchanges, Reason: r.Reason, SpokenAt: r.SpokenAt}
			switch r.Op {
			case "close":
				change.Status = r.Fields["status"]
				next.CloseCommitments = []CommitmentChange{change}
			case "merge":
				next.MergeCommitments = []CommitmentMerge{{Into: r.Fields["merged_into"], From: []string{m.ID}, Reason: r.Reason}}
			default:
				next.ReviseCommitments = []CommitmentChange{change}
			}
			u.commit(&next, -1, nil)
		}
	}
	for i, day := range d.Filed {
		day.Session = fmt.Sprintf("seed_day_%d", i)
		u.days = append(u.days, day)
	}
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
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
	l := fold(u.changesets, u.canon) // 重新叠加得到的是新的切片，调用方可以随意修改
	st := State{Projects: slices.Clone(u.projects), Exchanges: l.exchanges, Commitments: l.commitments, Changesets: slices.Clone(u.changesets),
		Days: u.sortedDays(), Weeks: slices.Clone(u.weeks), Merges: slices.Clone(u.merges)}
	slices.Reverse(st.Days)
	st.Projects, st.Exchanges, st.Commitments, st.Changesets = orEmpty(st.Projects), orEmpty(st.Exchanges), orEmpty(st.Commitments), orEmpty(st.Changesets)
	st.Days, st.Weeks, st.Merges = orEmpty(st.Days), orEmpty(st.Weeks), orEmpty(st.Merges)
	return st
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// user 返回业务线中 EndUser 的数据；调用方持有 mu。
func (x *Server) user(businessLine, endUser string) *user {
	k := businessLine + "\x00" + endUser
	u := x.users[k]
	if u == nil {
		u = &user{weekSessions: map[string]string{}, submitted: map[string]string{}, next: 101}
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

// refold 由变更集重新叠加出交谈与承诺。
func (u *user) refold() { u.ledger = fold(u.changesets, u.canon) }

// foldWithout 是去掉第 i 个变更集（将被取代的那个）之后的状态；i < 0 时就是当前状态。新的变更集据此检查：重交时它的
// rev 是对着没有旧变更集的账本取的。
func (u *user) foldWithout(i int) ledger {
	if i < 0 {
		return u.ledger
	}
	return fold(slices.Delete(slices.Clone(u.changesets), i, i+1), u.canon)
}

// canon 把已合并的项目换成保留的那个。
func (u *user) canon(id string) string {
	for range len(u.projects) {
		p := u.project(id)
		if p == nil || p.MergedInto == "" {
			break
		}
		id = p.MergedInto
	}
	return id
}

func (u *user) changesetSession(id string) string {
	if i := slices.IndexFunc(u.changesets, func(cs Changeset) bool { return cs.ID == id }); i >= 0 {
		return u.changesets[i].Session
	}
	return ""
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

// where 呈现一段交谈的归属。
func (u *user) where(e *Exchange) string {
	var parts []string
	for _, l := range e.Projects {
		switch {
		case l.Project != "":
			name := ""
			if p := u.project(l.Project); p != nil {
				name = " " + p.Name
			}
			parts = append(parts, "归入 "+l.Project+name)
		case l.Match == "unsure":
			parts = append(parts, "待用户确认：可能属于 "+l.Candidate)
		}
	}
	if len(parts) == 0 {
		return "不归项目"
	}
	return strings.Join(parts, "；")
}

func (u *user) projectLine(p *Project) string {
	open := 0
	for _, m := range u.commitments {
		if m.Project == p.ID && m.Open() {
			open++
		}
	}
	status := p.Status
	if status == "" {
		status = "（还没有现状）"
	}
	return fmt.Sprintf("%s %s：%s（未完成的承诺 %d 条）\n", p.ID, p.Name, status, open)
}

// commitmentLine 呈现一条承诺给模型：ID、rev、状态、内容与期限、归属、出处、引用与之后的修订（说话时间与依据）。
func (u *user) commitmentLine(m *Commitment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s（rev %d", m.ID, m.Rev())
	if m.MergedInto != "" {
		fmt.Fprintf(&b, "，已并入 %s", m.MergedInto)
	} else if m.Status != "open" {
		b.WriteString("，" + m.Status)
	}
	b.WriteString("）" + commitmentText(m))
	if m.Project != "" {
		b.WriteString("；归入 " + m.Project)
	}
	if m.Exchange != "" {
		b.WriteString("；出处 " + m.Exchange)
	}
	if len(m.Refs) > 0 {
		b.WriteString("；引用 " + strings.Join(m.Refs, "、"))
	}
	for _, r := range m.Revisions[1:] {
		at := r.RecordedAt
		if !r.SpokenAt.IsZero() {
			at = r.SpokenAt
		}
		fmt.Fprintf(&b, "；%s %s（依据%s）", at.Format("01-02 15:04"), revisionText(&r), r.Basis())
	}
	return b.String()
}

func revisionText(r *Revision) string {
	switch {
	case r.Op == "merge":
		return "并入 " + r.Fields["merged_into"]
	case r.Fields["status"] != "":
		return "标为 " + r.Fields["status"]
	case r.Fields["due_date"] != "":
		return "期限改为 " + r.Fields["due_date"]
	}
	return "改了 " + strings.Join(sortedKeys(r.Fields), "、")
}

// openCommitments 列出项目未完成的承诺（带 rev），供 Project 的 Run 对账。
func (u *user) openCommitments(b *strings.Builder, project string) {
	first := true
	for i := range u.commitments {
		m := &u.commitments[i]
		if m.Project != project || !m.Open() {
			continue
		}
		if first {
			b.WriteString("\n未完成的承诺：\n")
			first = false
		}
		fmt.Fprintf(b, "- %s（rev %d）%s\n", m.ID, m.Rev(), commitmentText(m))
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

// exchangeText 把一段交谈呈现为给 Project 的 Run 与助手看的文字；project 非空时只列属于它（或没有标明项目）的决定。
func exchangeText(e *Exchange, project string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", e.Summary)
	var ds []string
	for _, d := range e.Decisions {
		if project == "" || d.Project == "" || d.Project == project {
			ds = append(ds, d.Text)
		}
	}
	list(&b, "已定", ds)
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
