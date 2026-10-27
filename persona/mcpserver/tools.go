package mcpserver

import (
	"encoding/json"
	"persona/domain"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

const listCommitmentsSchema = `{"type":"object","properties":{"kind":{"type":"string","enum":["mine","theirs"]},"status":{"type":"string","enum":["open","done","cancelled","all"],"description":"默认 open；查修订历史时用 all"},"due_from":{"type":"string","description":"期限下限 YYYY-MM-DD（含当天）"},"due_to":{"type":"string","description":"期限上限 YYYY-MM-DD（含当天）"},"counterpart":{"type":"string","description":"对方称呼的子串"},"query":{"type":"string","description":"主题，按内容与对方匹配"},"project":{"type":"string","description":"项目 ID"},"after":{"type":"string","description":"照抄上一页末尾的 after，其他条件保持不变"}}}`

var tools = []struct {
	tool    *mcp.Tool
	handler func(*domain.Call) string
}{
	{&mcp.Tool{Name: "list_commitments", Description: "按责任方、状态、期限、对方、主题与项目查询承诺，默认未完成；每页至多 30 条，按期限升序，无期限排后。被合并的不列出。", InputSchema: json.RawMessage(listCommitmentsSchema), Annotations: readOnly()}, (*domain.Call).ListCommitments},
	{&mcp.Tool{Name: "commitment_history", Description: "查询一条承诺的当前值与修订历史（改前到改后、时间、依据、引用），以及合并关系。", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"承诺 ID"}},"required":["id"]}`), Annotations: readOnly()}, (*domain.Call).CommitmentHistory},
	{&mcp.Tool{Name: "list_projects", Description: "列出用户已有的项目（ID、名称与现状）。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()}, (*domain.Call).ListProjects},
	{&mcp.Tool{Name: "find_related", Description: "为一段交谈召回相关的数据：至多 5 个候选项目（ID、名称、现状），以及与这段的对方或主题相关的" +
		"未完成承诺（不限哪天，带 ID 与 rev），供判断归属，以及这段里的原话是否改动了已有的承诺。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"交谈的主题，如\"新办公室装修 报价\""},` +
			`"counterparts":{"type":"array","items":{"type":"string"},"description":"交谈的对方，按原话称呼，如 [\"张总\"]"}},"required":["query"]}`),
		Annotations: readOnly()}, (*domain.Call).FindRelated},
	{&mcp.Tool{Name: "project_digest", Description: "返回一个项目的现状、按天的时间线与未完成的承诺。在项目自己的会话中可以不填 project。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","description":"项目 ID；项目会话中省略即本项目"}}}`),
		Annotations: readOnly()}, (*domain.Call).ProjectDigest},
	{&mcp.Tool{Name: "recent_days", Description: "返回最近几天（按日期）的整理摘要与交谈。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()}, (*domain.Call).RecentDays},
	{&mcp.Tool{Name: "file_changes", Description: "交出一次整理的变更集：这次读到的旁听里划出的交谈、新的承诺，以及这些原话对已有承诺的改动" +
		"（改期、改内容、完成、作废），一次调用交出全部。每项都用编号引用依据的原话，业务线按被引用原话的说话人核对承诺人、按说话那天核对期限；" +
		"整体检查，有一项不通过就整体不收，结果写明原因。同一个 source 再交一次时整体取代上一次。" +
		"projects 是归属：existing（明确是 find_related 召回的某个项目，填 id）、new（值得持续跟进的新主题，填 name）、" +
		"unsure（可能是某个候选但拿不准：填候选的 id，再给一个新项目名 name，交给用户确认）、none（一次性的事，不归项目）。" +
		"白天的整理只能依据本次读到的原话改动已有的承诺；合并重复的承诺与交谈、改正交谈的归属只在当天整理（source 为 {\"day\": true}）中做。",
		InputSchema: json.RawMessage(fileChangesSchema)}, (*domain.Call).FileChanges},
	{&mcp.Tool{Name: "day_ledger", Description: "返回今天的账本，供当天整理与写当天总览：今天的交谈（ID、范围、标题、摘要、归属、沟通方式的观察）、" +
		"今天的承诺与之前未完成而与今天相关的承诺（带 ID、rev、引用与修订），以及按规则找出的可能重复的承诺、可能被切开的交谈。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()}, (*domain.Call).DayLedger},
	{&mcp.Tool{Name: "file_day", Description: "交出当天的总览与觉察，在\"整理今天\"中读完全部旁听、交出变更集之后调用一次。day 是当天的总览；" +
		"awareness 是对用户本人沟通方式的观察与建议。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"day":{"type":"object","description":"当天的总览","properties":{` +
			`"headline":{"type":"string","description":"一句话概括这一天"},` +
			`"highlights":{"type":"array","items":{"type":"string"},"description":"当天最重要的几件事（3 条以内）"}},"required":["headline"]},` +
			`"awareness":{"type":"object","description":"对用户本人沟通方式的观察；不写情绪与身心状态","properties":{` +
			`"habits":{"type":"array","items":{"type":"string"},"description":"沟通习惯，各附当天的一个例子"},` +
			`"suggestions":{"type":"array","items":{"type":"string"},"description":"下次可以怎么做"}}}},` +
			`"required":["day"]}`)}, (*domain.Call).FileDay},
	{&mcp.Tool{Name: "day_summary", Description: "取回某天归入本项目的交谈（带交谈 ID），以及本项目当前未完成的承诺（带 ID 与 rev）。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","description":"日期 YYYY-MM-DD"}},"required":["date"]}`),
		Annotations: readOnly()}, (*domain.Call).DaySummary},
	{&mcp.Tool{Name: "set_status", Description: "更新本项目的现状（整体替换）：现在进展到哪、下一步、待解决的问题。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","description":"现状，两三句话"},` +
			`"next_steps":{"type":"array","items":{"type":"string"}},"open_questions":{"type":"array","items":{"type":"string"}}},"required":["status"]}`)}, (*domain.Call).SetStatus},
	{&mcp.Tool{Name: "update_commitments", Description: "依据交谈对账本项目的承诺：close 是已完成（done）或取消（cancelled）的，revise 是改了期限或内容的；" +
		"每项带 day_summary 给出的 rev，并用 exchanges 写明依据的交谈 ID。账本在你看到之后又被改过（rev 不符）时整体不收，结果附上现在的状态。" +
		"同一天再交一次时整体取代上一次。新的承诺不在这里添加。",
		InputSchema: json.RawMessage(updateCommitmentsSchema)}, (*domain.Call).UpdateCommitments},
	{&mcp.Tool{Name: "week_overview", Description: "返回最近 7 天每天的总览、交谈的标题与归属，以及每天对用户沟通方式的觉察。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()}, (*domain.Call).WeekOverview},
	{&mcp.Tool{Name: "similar_projects", Description: "找出至多 5 对可能重复的项目（相似度从高到低），附各自的现状与最近几段交谈的标题，供判断是否合并。",
		InputSchema: json.RawMessage(noArgs), Annotations: readOnly()}, (*domain.Call).SimilarProjects},
	{&mcp.Tool{Name: "propose_merge", Description: "提议把项目 from 并入 into（两者指同一件事）。提议进业务线的\"待确认\"，由用户确认后才合并。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"into":{"type":"string","description":"保留的项目 ID"},` +
			`"from":{"type":"string","description":"并入的项目 ID"},"reason":{"type":"string","description":"为什么是同一件事"}},"required":["into","from","reason"]}`)}, (*domain.Call).ProposeMerge},
	{&mcp.Tool{Name: "file_week", Description: "交出本周的觉察，每次回顾调用一次：habits 是本周至少在两天出现过的沟通习惯（days 写出现的日期，example 附一个例子），" +
		"suggestions 是建议。只出现一天的不算习惯。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"habits":{"type":"array","items":{"type":"object","properties":{` +
			`"habit":{"type":"string"},"days":{"type":"array","items":{"type":"string"},"description":"出现的日期 YYYY-MM-DD，至少两天"},` +
			`"example":{"type":"string"}},"required":["habit","days"]}},"suggestions":{"type":"array","items":{"type":"string"}}},"required":["habits"]}`)}, (*domain.Call).FileWeek},
}
