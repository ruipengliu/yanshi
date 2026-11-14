// Package eval 运行评测用例集并与基线比较（docs/design/m4-eval.md，ADR-0018）。
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"yanshi/internal/eval/xinlifixture"
)

// Case 是一个评测用例（evals/*.yaml）。
type Case struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Agent       string `yaml:"agent"`
	Oneshot     string `yaml:"oneshot"`
	Trials      int    `yaml:"trials"`
	// TimeZone 覆盖 Session 时区；xinli 用例默认上海，其他用例保留无时区。
	TimeZone string `yaml:"time_zone"`
	// Timeout 是每个原轮次的等待上限，拆 Run 时共享，默认取 Config.TurnTimeout（长 Run 用例需要更长）。
	Timeout time.Duration `yaml:"timeout"`
	// Requires 列出所需的环境，如 "sandbox"；不满足时跳过。
	Requires []string `yaml:"requires"`
	// Capabilities 追加到被评测 AgentDef 的能力白名单（如 "mcp:crm/*"）。
	Capabilities []string `yaml:"capabilities"`
	// Context 在 Window 非零时覆盖窗口、保留量与结果上限，后两者为零时取窗口默认；其余配置继承 AgentDef。
	Context struct {
		Window        int `yaml:"window"`
		KeepRecent    int `yaml:"keep_recent"`
		MaxToolResult int `yaml:"max_tool_result"`
	} `yaml:"context"`
	// Call 为 true 时用例在一次 Call 中进行（docs/design/m3-call.md §9）：每轮的 input 合成为语音说出，
	// 由实时语音模型应答、经 run_task 派生任务。需要实时语音模型与语音合成，不满足时跳过。
	Call bool `yaml:"call"`
	// GoldFile 是用例目录下的金标文件（m4-eval.md §13）：旁听中标了的承诺应有的样子与之后的改动，expect.xinli.gold
	// 据此计算指标。Noise 为 true 时提交旁听识别有误的版本（CaptureSegment 的 asr、heard），金标不变：与不带噪声的
	// 同一份数据对照，衡量识别错误的影响。
	GoldFile string `yaml:"gold"`
	Noise    bool   `yaml:"noise"`
	// XinliSpanRuns 在评测中实验每个完整时段一个 Run，原轮次的断言与金标仍只检查一次（m4-eval.md §14）。
	XinliSpanRuns bool `yaml:"xinli_span_runs"`
	Setup         struct {
		Memories []struct {
			Category string `yaml:"category"`
			Content  string `yaml:"content"`
			// Recorded 是记录日期（YYYY-MM-DD），用于预置新旧不同的记忆；默认为当前时间。
			Recorded string `yaml:"recorded"`
			// ValidUntil 是有效期的最后一天（须是将来）；ExpiredOn 是已经过去的有效期最后一天（预置已过期的计划）。
			ValidUntil string `yaml:"valid_until"`
			ExpiredOn  string `yaml:"expired_on"`
			// ReplacesPrevious 为 true 时这条取代列表中的上一条（上一条作为历史保留，m4-memory-grant.md §11）。
			ReplacesPrevious bool `yaml:"replaces_previous"`
		} `yaml:"memories"`
		Device *DeviceSetup `yaml:"device"`
		// Devices 是另外几台设备（标签互不相同），用于检验同类能力有多台设备时的选择（docs/design/m6-decisions.md §3）。
		Devices []DeviceSetup `yaml:"devices"`
		// CRM 为 true 时为评测业务线登记内置的 CRM MCP Server（工具 search_customer、create_ticket）。
		CRM bool `yaml:"crm"`
		// Agents 是本用例可委托的 Agent（docs/design/m5-multi-agent.md）：与被评测的 Agent 使用同一模型；
		// 名称在整个评测集中唯一，被评测的 Agent 须在 capabilities 中以 "agent:<名称>" 允许委托。
		Agents []AgentSetup `yaml:"agents"`
		// Xinli 非 nil 时为评测业务线启动外部 xinli 夹具并登记 MCP Server（docs/design/xinli-captures.md §1），并为本次运行的 EndUser
		// 预置其中的项目与每日摘要。
		Xinli *XinliSetup `yaml:"xinli"`
		// NoHealth 为 true 时本用例在未开启健康信息的业务线中运行（ADR-0022）：health 类别的写入被拒绝。
		NoHealth bool `yaml:"no_health"`
	} `yaml:"setup"`
	Turns []Turn `yaml:"turns"`

	gold *Gold
}

// AgentSetup 是评测用的被委托 Agent。
type AgentSetup struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Instructions string   `yaml:"instructions"`
	Capabilities []string `yaml:"capabilities"`
}

// XinliSetup 是 xinli MCP Server 中预置的数据：已有的项目、按日期的摘要（day_summary 在没有交出的交谈时返回它）与承诺账本。
type XinliSetup struct {
	Projects    []XinliProject    `yaml:"projects"`
	Days        map[string]string `yaml:"days"`
	Commitments []XinliCommitment `yaml:"commitments"`
	// Filed 是已经交出的每日整理（date、headline、awareness），Exchanges 是已经交出的交谈（date、title、summary、project、
	// decisions、risks）：每周回顾与重写现状读取它们（xinli-captures.md §10）。日期可写成相对于运行当天的
	// "today-N"（"本周"由运行时刻决定）。
	Filed     []XinliDay      `yaml:"filed"`
	Exchanges []XinliExchange `yaml:"exchanges"`
	// ProjectSession 是一个预置项目的 ID：本用例的 Session 就是这个项目的 xinli-project Session（如同业务线归集时创建的）。
	ProjectSession string `yaml:"project_session"`
}

type (
	XinliProject    = xinlifixture.Project
	XinliCommitment = xinlifixture.SeedCommitment
	XinliDay        = xinlifixture.Day
	XinliExchange   = xinlifixture.SeedExchange
)

// XinliExpect 是对 xinli 业务线数据的断言。
type XinliExpect struct {
	// Exchanges 是整理交出、仍然有效的交谈数的范围（不含预置的，被并入别处的不算）；Pending 是"待确认"中交谈数的范围；
	// Projects 是项目数（含预置的）的范围。
	Exchanges *Range `yaml:"exchanges"`
	Pending   *Range `yaml:"pending"`
	Projects  *Range `yaml:"projects"`
	// InProject 是关联到各项目（按 ID）的交谈数的范围，如 {P-0001: {max: 0}}；一段交谈可以关联几个项目。
	InProject map[string]Range `yaml:"in_project"`
	// Commitments 中每项须匹配账本中的某条承诺；NoCommitments 中每项都不得匹配任何一条。
	Commitments   []CommitmentMatch `yaml:"commitments"`
	NoCommitments []CommitmentMatch `yaml:"no_commitments"`
	// CommitmentsCount 是整理交出的承诺数（不含预置的与被合并的）的范围：承诺过多时提醒会被淹没（F5）。
	CommitmentsCount *Range `yaml:"commitments_count"`
	// StaleOpen 是过时的未完成承诺：Of 中每项描述一条改期、作废或转交之前的旧版本，仍未完成且匹配其中某项的至多 Max 条（F1）。
	StaleOpen *StaleExpect `yaml:"stale_open"`
	// Grouping 是项目归集的碎片率与纯度（xinli-captures.md §10、m4-eval.md），需要旁听标上主题（CaptureSegment.Topic）。
	Grouping *GroupingExpect `yaml:"grouping"`
	// Covers 是摘要须覆盖的要点：评分模型逐条判断要点是否出现在交出的交谈与总览中，覆盖率至少为 Min。
	Covers *CoversExpect `yaml:"covers"`
	// VerifiedRate 是随交谈交出的承诺中通过核对的比例下限（引用准确率）。
	VerifiedRate *float64 `yaml:"verified_rate"`
	// Merges 中每对项目须有合并提议（顺序不限）；NoMerges 中每对都不得有。
	Merges   [][2]string `yaml:"merges"`
	NoMerges [][2]string `yaml:"no_merges"`
	// WeekHabits 中每项须出现在某次每周回顾的习惯中；NoWeekHabits 中每项都不得出现。
	WeekHabits   []string `yaml:"week_habits"`
	NoWeekHabits []string `yaml:"no_week_habits"`
	// Status 是项目现状的断言：按项目 ID，contains 中的都要出现，excludes 中的都不得出现。
	Status map[string]TextMatch `yaml:"status"`
	// Gold 非 nil 时按用例的金标计算本轮的指标（goldMetrics，m4-eval.md §13）并记入报告；其中给出的是指标的下限，
	// 未给出的只记录、不判定。写成 {} 即只记录。
	Gold map[string]float64 `yaml:"gold"`
}

// StaleExpect 是过时的未完成承诺数的断言。
type StaleExpect struct {
	Of  []CommitmentMatch `yaml:"of"`
	Max int               `yaml:"max"`
}

// TextMatch 要求文本包含 Contains 中的全部、不含 Excludes 中的任何一项。
type TextMatch struct {
	Contains []string `yaml:"contains"`
	Excludes []string `yaml:"excludes"`
}

// GroupingExpect 是项目归集的断言。
type GroupingExpect struct {
	// Fragmentation 是每个主题平均分散在几个项目中（理想为 1）；Purity 是项目中占多数的主题的比例下限（理想为 1）。
	Fragmentation *FloatRange `yaml:"fragmentation"`
	Purity        *float64    `yaml:"purity"`
	// Topics 是至少应交出的主题数：主题根本没有交出时碎片率与纯度都无从谈起。
	Topics int `yaml:"topics"`
}

type FloatRange struct {
	Min *float64 `yaml:"min"`
	Max *float64 `yaml:"max"`
}

// CoversExpect 是摘要覆盖率的断言。
type CoversExpect struct {
	Points []string `yaml:"points"`
	Min    float64  `yaml:"min"`
}

// CommitmentMatch 匹配承诺账本中的一条：未写的字段不检查。
type CommitmentMatch struct {
	Kind     string `yaml:"kind"` // mine | theirs
	Contains string `yaml:"contains"`
	// DueDate 要求期限的日期（YYYY-MM-DD）；Status 是 open（未完成且没有被合并）、done、cancelled 或 merged；Project 是所属项目 ID。
	DueDate string `yaml:"due_date"`
	Status  string `yaml:"status"`
	Project string `yaml:"project"`
	// Verified 非 nil 时要求核对的结果：true 为通过核对，false 为未核实。
	Verified *bool `yaml:"verified"`
	// Rev 非零时要求修订数（新建算 1）：改期应修订同一条，而不是新建。
	Rev int `yaml:"rev"`
}

// CaptureSegment 是一段旁听转写；业务侧元数据不经过内核 proto，以免丢失身份与核对依据。
type CaptureSegment struct {
	Speaker           string                  `yaml:"speaker"`
	Key               string                  `yaml:"key"`
	SpeakerRef        string                  `yaml:"speaker_ref"`
	Text              string                  `yaml:"text"`
	SpeakerGrade      string                  `yaml:"speaker_grade"`
	Disposition       string                  `yaml:"disposition"`
	DispositionReason string                  `yaml:"disposition_reason"`
	Background        bool                    `yaml:"background"`
	LowSupportSpans   []CaptureLowSupportSpan `yaml:"low_support_spans"`
	// At 是说这段话的当地时间（"2026-10-23 09:42"，Asia/Shanghai），或相对于提交时刻的 "now-15m"；为空时不带时间，
	// 整理时也不显示。
	At string `yaml:"at"`
	// Topic 是这段话的主题标签（评测用，不提交给 yanshi）：计算项目归集的纯度与碎片率。闲聊不标。
	Topic string `yaml:"topic"`
	// Gold 是这段话所关于的金标承诺 ID（以空格分隔，评测用，不提交）：答应、改期、完成或改由他人去做的原话都标上。
	Gold string `yaml:"gold"`
	// ASR、Heard 与 HeardKey 是识别有误的版本（Case.Noise 时提交）：转写的文字、识别成的说话人与他人的 key；为空时
	// 与原文相同。
	ASR      string `yaml:"asr"`
	Heard    string `yaml:"heard"`
	HeardKey string `yaml:"heard_key"`
}

// heard 返回识别有误的版本。
func (g CaptureSegment) heard() CaptureSegment {
	speakerChanged := g.Heard != "" && g.Heard != g.Speaker
	g.LowSupportSpans = slices.Clone(g.LowSupportSpans)
	for i := range g.LowSupportSpans {
		g.LowSupportSpans[i].CriticalKinds = slices.Clone(g.LowSupportSpans[i].CriticalKinds)
	}
	if g.ASR != "" {
		g.Text = g.ASR
	}
	if g.Heard != "" {
		g.Speaker = g.Heard
	}
	if g.HeardKey != "" {
		g.Key = g.HeardKey
	}
	if speakerChanged {
		if g.Speaker != "wearer" {
			g.SpeakerGrade = ""
		}
		if g.Speaker != "other" {
			g.Key, g.SpeakerRef = "", ""
		}
	}
	return g
}

// DeviceSetup 是虚拟设备：只读的文件、需审批的写文件与发消息。
type DeviceSetup struct {
	Label string `yaml:"label"`
	// Kind 是设备类型：pc（默认：文件、写文件、发消息）、phone（闹钟、日程、音乐、定位）或 car（导航、空调、音乐）。
	// 手机与车机的能力只记录调用并返回成功，用于检验选对设备、选对工具与卡片展示。
	Kind  string            `yaml:"kind"`
	Files map[string]string `yaml:"files"`
	// FilesFrom 从评测集目录下的文件读取设备文件内容（大文件），键为设备上的路径。
	FilesFrom map[string]string `yaml:"files_from"`
	// OnlineAfter > 0 时设备在本次运行开始后这么久才上线：检验设备离线时 Run 挂起、上线后恢复。
	OnlineAfter time.Duration `yaml:"online_after"`
}

type Turn struct {
	// Redo 复制上一轮的输入，以那一轮的 Run ID 作比较并替换。
	Redo         bool `yaml:"redo"`
	inputPresent bool
	// AdvanceToNextSchedule 不提交输入，让已有定时经 Worker 到点开启 Run。
	AdvanceToNextSchedule bool `yaml:"advance_to_next_schedule"`
	// Scheduled 通过真实定时入口提交本轮文字，验证来源与到点标记。
	Scheduled bool   `yaml:"scheduled"`
	Input     string `yaml:"input"`
	// FromDevice 是用户发这条输入时所在的设备标签，与网关为经 Connection 的输入注明的一致。
	FromDevice string `yaml:"from_device"`
	// UIContext 随输入一并提交的界面上下文（docs/design/m3-duplex-channel.md §8）。
	UIContext *struct {
		Screen    string `yaml:"screen"`
		Ref       string `yaml:"ref"`
		Selection string `yaml:"selection"`
		Content   string `yaml:"content"`
	} `yaml:"ui_context"`
	// Approve 是本轮遇到审批时的决定，默认批准。
	Approve *bool `yaml:"approve"`
	// NewSession 为 true 时本轮在新的 Session 中进行（同一 EndUser）；Agent 非空时新 Session 使用它（默认为用例的
	// agent），如整理几天之后向 xinli-assistant 提问。
	NewSession bool   `yaml:"new_session"`
	Agent      string `yaml:"agent"`
	// CrashAfterCalls > 0 时，本轮的 Run 发起这么多次调用后，"杀掉"正在执行它的 Worker（取消其上下文，
	// 不释放租约），由其他 Worker 在租约到期后接管：用真实模型检验接管后能否接着完成。
	CrashAfterCalls int `yaml:"crash_after_calls"`
	// CardAction 非空时本轮不提交文字，而是在 Session 最近展示的卡片（或 Card 指定的卡片）上执行动作
	// （docs/design/cards.md §4）。
	CardAction *CardActionScript `yaml:"card_action"`
	// Answer 是本轮遇到 ask_user 提问时的回答方式；为空时本轮停在提问处（状态 asked），
	// 下一轮的输入即作为回答（与用户直接打字回答一致）。
	Answer *AnswerScript `yaml:"answer"`
	// AwaitBackground 为 true 时本轮不提交输入，而是等待后台调用的了结（ADR-0035）：设备上线后送达的结果（或到期）
	// 作为一条输入开启新 Run（或并入进行中的 Run），本轮检查的是那个 Run。须与 Input 二选一。
	AwaitBackground bool `yaml:"await_background"`
	// Captures 是本轮输入之前提交的一批旁听：交给已绑定的 Persona 业务日；
	// 跨天的用例在 new_session 的一轮提交新一天的旁听；历史 at 保留，业务日仍共用本次运行当天。
	Captures []CaptureSegment `yaml:"captures"`
	// CapturesFile 是用例目录下的数据文件（YAML 的旁听列表，格式同 captures），追加在 Captures 之后：长的一天不写在用例里。
	CapturesFile string `yaml:"captures_file"`
	// JudgeState 只为本轮评分附加最终业务快照，旧用例的评分输入保持原样。
	JudgeState bool `yaml:"judge_state"`
	// XinliEdit 是本轮旁听与输入之前用户在 App 中对承诺的改动（按金标 ID 找到对应的承诺）。
	XinliEdit []GoldEdit `yaml:"xinli_edit"`
	Expect    Expect     `yaml:"expect"`
}

// UnmarshalYAML 保留 input 的存在性：显式空输入也不能与重答混用。
func (t *Turn) UnmarshalYAML(node *yaml.Node) error {
	type plain Turn
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*t = Turn(value)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "input" {
			t.inputPresent = true
		}
	}
	return nil
}

// CardActionScript 模拟用户在卡片上点按钮或点选列表项。Action 为空时点第一个按钮；Label 非空时点标签包含它的按钮或条目。
type CardActionScript struct {
	Card   string            `yaml:"card"`
	Action string            `yaml:"action"`
	Label  string            `yaml:"label"`
	Values map[string]string `yaml:"values"`
}

// AnswerScript 模拟用户回答 ask_user：点选标签含 Choose 的选项、填写 Values、或输入 Text；
// Reply 非空时改为在对话中直接打字回答（经提交输入，而不是回答接口）。
type AnswerScript struct {
	Choose string            `yaml:"choose"`
	Values map[string]string `yaml:"values"`
	Text   string            `yaml:"text"`
	Reply  string            `yaml:"reply"`
}

type Range struct {
	Min *int `yaml:"min"`
	Max *int `yaml:"max"`
}

type MemoryMatch struct {
	// Category 以 "!" 开头时匹配其他类别，如 "!health"：健康信息不应被改写到其他类别。
	Category string `yaml:"category"`
	Contains string `yaml:"contains"`
	// Without 非空时，包含它的记忆不算匹配。例如更正后旧地址可以作为历史出现在新记忆里，
	// 但不应有只含旧地址的记忆：{contains: 旧, without: 新}。
	Without string `yaml:"without"`
	// Max 非零时（只用于 memories），匹配的记忆至多这么多条：同一事实不应一再写入。
	Max int `yaml:"max"`
	// HasValidUntil 为 true 时（只用于 memories）要求带有效期（m4-memory-grant.md §11）。
	HasValidUntil bool `yaml:"has_valid_until"`
}

// CallMatch 匹配调用名称与参数中的文字；顺序断言将参数压紧为 JSON，避免空白差异。
type CallMatch struct {
	Capability string   `yaml:"capability"`
	Contains   []string `yaml:"contains"`
}

// CallResultMatch 按调用名称与完整结果文本计数，避免评分摘要的截断掩盖拒收回执。
type CallResultMatch struct {
	Capability string `yaml:"capability"`
	Contains   string `yaml:"contains"`
	Count      *Range `yaml:"count"`
}

// ScheduleExpect 只检查投影中仍有 NextDue 的定时。
type ScheduleExpect struct {
	Active        *Range   `yaml:"active"`
	RRuleContains []string `yaml:"rrule_contains"`
	TimeZone      string   `yaml:"time_zone"`
}

// Expect 是一轮的断言；未写的字段不检查。
type Expect struct {
	OutputSchemaValid *bool `yaml:"output_schema_valid"`
	// CallSequence 按顺序匹配不同调用，允许其间有其他调用。
	CallSequence []CallMatch       `yaml:"call_sequence"`
	CallResults  []CallResultMatch `yaml:"call_results"`
	Schedules    *ScheduleExpect   `yaml:"schedules"`
	// Status 是本轮结束时 Run 的状态，可用 "|" 列出多个可接受的值，如 "completed|asked"
	// （asked：停在 ask_user 提问处等待回答）。默认 completed。
	Status  string   `yaml:"status"`
	Calls   []string `yaml:"calls"`
	NoCalls []string `yaml:"no_calls"`
	// MaxCalls 限制名称匹配（glob）的调用在本轮出现的次数，如 {"macbook__read_file": 1}：调用转入后台后不应重试。
	MaxCalls  map[string]int `yaml:"max_calls"`
	Approvals *Range         `yaml:"approvals"`
	// Questions 是本轮 ask_user 提问次数的范围。
	Questions     *Range   `yaml:"questions"`
	ReplyContains []string `yaml:"reply_contains"`
	// ReplyExcludes 中的文本都不得出现在回复中（如过期的计划不该被当成之后的安排）。
	ReplyExcludes []string          `yaml:"reply_excludes"`
	ReplyMaxChars int               `yaml:"reply_max_chars"`
	Memories      []MemoryMatch     `yaml:"memories"`
	NoMemories    []MemoryMatch     `yaml:"no_memories"`
	DeviceWrites  map[string]string `yaml:"device_writes"`
	DeviceSent    *int              `yaml:"device_sent"`
	// CallArgs 要求某个名称匹配（glob）的调用的参数包含给定文本，如 {"*send_message": "lilei@example.com"}。
	CallArgs map[string]string `yaml:"call_args"`
	// NoCallArgs 要求名称匹配（glob）的调用的参数都不包含给定文本，如 {"mcp_xinli__file_day": "合同"}。
	NoCallArgs map[string]string `yaml:"no_call_args"`
	// Compacted 要求到本轮结束时 Session 已发生过上下文压缩（压缩类用例据此确认确实测到了压缩）。
	Compacted bool `yaml:"compacted"`
	// Compactions 与 Takeovers 是本轮 Run 内上下文压缩与接管次数的范围（长 Run 用例）。
	Compactions *Range `yaml:"compactions"`
	Takeovers   *Range `yaml:"takeovers"`
	// Tasks 是本轮 Call 中派生任务（run_task）次数的范围；TaskContains 要求某个任务的内容包含给定文本。
	Tasks        *Range   `yaml:"tasks"`
	TaskContains []string `yaml:"task_contains"`
	// Xinli 是本轮之后 xinli MCP Server 中本 EndUser 的数据应满足的条件（docs/design/xinli-captures.md §10）。
	Xinli *XinliExpect `yaml:"xinli"`
	// Tickets 是本轮之后 CRM 中本 EndUser 的工单应包含的文本（每项须出现在某个工单中）。
	Tickets []string `yaml:"tickets"`
	Judge   string   `yaml:"judge"`
}

var caseNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// LoadSuite 读取 dir 下所有 *.yaml 用例，按名称排序。
func LoadSuite(dir string) ([]*Case, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var out []*Case
	seen, agents := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		c := &Case{}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := validateOneshotCase(c); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if !caseNameRE.MatchString(c.Name) || seen[c.Name] {
			return nil, fmt.Errorf("%s: case name %q missing, invalid or duplicated", f, c.Name)
		}
		if c.TimeZone != "" {
			if _, err := time.LoadLocation(c.TimeZone); err != nil || c.TimeZone == "Local" {
				return nil, fmt.Errorf("%s: invalid time_zone %q", f, c.TimeZone)
			}
		}
		if len(c.Turns) == 0 {
			return nil, fmt.Errorf("%s: no turns", f)
		}
		if c.XinliSpanRuns && (c.Agent != "xinli-day" || c.Setup.Xinli == nil || c.Call) {
			return nil, fmt.Errorf("%s: xinli_span_runs needs agent xinli-day and setup.xinli, without call", f)
		}
		if c.GoldFile != "" {
			if c.Setup.Xinli == nil {
				return nil, fmt.Errorf("%s: a gold file needs setup.xinli", f)
			}
			b, err := os.ReadFile(filepath.Join(dir, c.GoldFile))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			c.gold = &Gold{}
			if err := yaml.Unmarshal(b, c.gold); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", f, c.GoldFile, err)
			}
			if err := c.gold.validate(); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", f, c.GoldFile, err)
			}
		}
		labelled := map[string]bool{}
		for i, t := range c.Turns {
			for j, want := range t.Expect.CallResults {
				if err := validateCallResultMatch(want); err != nil {
					return nil, fmt.Errorf("%s: turn %d: call_results %d: %w", f, i+1, j+1, err)
				}
			}
			if t.Redo && (i == 0 || t.inputPresent || t.Input != "" || t.Scheduled || t.AdvanceToNextSchedule || t.AwaitBackground || t.CardAction != nil || t.UIContext != nil || t.FromDevice != "" || t.NewSession || len(t.Captures) > 0 || t.CapturesFile != "" || len(t.XinliEdit) > 0 || c.Call) {
				return nil, fmt.Errorf("%s: turn %d redo requires a previous turn and cannot also submit input or start a session", f, i+1)
			}
			if t.Agent != "" && i > 0 && !t.NewSession {
				return nil, fmt.Errorf("%s: turn %d sets an agent without starting a new session", f, i+1)
			}
			if t.AdvanceToNextSchedule && (t.Input != "" || t.Scheduled || t.AwaitBackground || t.CardAction != nil || t.UIContext != nil || t.FromDevice != "" || t.NewSession || len(t.Captures) > 0 || t.CapturesFile != "" || len(t.XinliEdit) > 0 || c.Call) {
				return nil, fmt.Errorf("%s: turn %d advance_to_next_schedule cannot also submit input or start a session", f, i+1)
			}
			if t.Scheduled && (t.Input == "" || t.AwaitBackground || t.CardAction != nil || t.UIContext != nil || t.FromDevice != "" || c.Call) {
				return nil, fmt.Errorf("%s: turn %d scheduled input must be plain text", f, i+1)
			}
			if t.AwaitBackground && (t.Input != "" || t.CardAction != nil || t.NewSession || c.Call) {
				return nil, fmt.Errorf("%s: turn %d awaits a background result and cannot also submit input", f, i+1)
			}
			if t.CapturesFile != "" {
				b, err := os.ReadFile(filepath.Join(dir, t.CapturesFile))
				if err != nil {
					return nil, fmt.Errorf("%s: turn %d: %w", f, i+1, err)
				}
				var more []CaptureSegment
				if err := yaml.Unmarshal(b, &more); err != nil {
					return nil, fmt.Errorf("%s: turn %d: %s: %w", f, i+1, t.CapturesFile, err)
				}
				c.Turns[i].Captures = append(c.Turns[i].Captures, more...)
				t = c.Turns[i]
			}
			for j, g := range t.Captures {
				if err := validateCapture(g); err != nil {
					return nil, fmt.Errorf("%s: turn %d: capture %d: %w", f, i+1, j+1, err)
				}
				for _, id := range strings.Fields(g.Gold) {
					if c.gold == nil || !slices.ContainsFunc(c.gold.Commitments, func(m GoldCommitment) bool { return m.ID == id }) {
						return nil, fmt.Errorf("%s: turn %d: capture labelled with unknown gold commitment %q", f, i+1, id)
					}
					labelled[id] = true
				}
				if _, err := captureTime(g.At); g.At != "" && err != nil {
					return nil, fmt.Errorf("%s: turn %d: capture at %q must be YYYY-MM-DD HH:MM or now-<duration>", f, i+1, g.At)
				}
			}
			for _, e := range t.XinliEdit {
				if c.gold == nil || !slices.ContainsFunc(c.gold.Commitments, func(m GoldCommitment) bool { return m.ID == e.Gold }) {
					return nil, fmt.Errorf("%s: turn %d: xinli_edit of unknown gold commitment %q", f, i+1, e.Gold)
				}
				if err := goldDate(e.Due); err != nil || (e.Due == "") == (e.Status == "") || (e.Status != "" && e.Status != "done" && e.Status != "cancelled") {
					return nil, fmt.Errorf("%s: turn %d: xinli_edit sets either a due date or status done/cancelled", f, i+1)
				}
			}
			if x := t.Expect.Xinli; x != nil && x.Gold != nil {
				if c.gold == nil {
					return nil, fmt.Errorf("%s: turn %d: expect.xinli.gold needs a gold file", f, i+1)
				}
				for k := range x.Gold {
					if !slices.Contains(goldMetrics, k) {
						return nil, fmt.Errorf("%s: turn %d: unknown gold metric %q (one of %v)", f, i+1, k, goldMetrics)
					}
				}
			}
		}
		if c.gold != nil {
			for _, m := range c.gold.Commitments {
				if !labelled[m.ID] {
					return nil, fmt.Errorf("%s: gold commitment %q is not labelled on any capture", f, m.ID)
				}
			}
		}
		for _, a := range c.Setup.Agents {
			if agents[a.Name] {
				return nil, fmt.Errorf("%s: agent %q is defined by another case", f, a.Name)
			}
			agents[a.Name] = true
		}
		if c.Trials <= 0 {
			c.Trials = 3
		}
		if c.Agent == "" && c.Oneshot == "" {
			c.Agent = "assistant"
		}
		if d := c.Setup.Device; d != nil {
			for path, src := range d.FilesFrom {
				b, err := os.ReadFile(filepath.Join(dir, src))
				if err != nil {
					return nil, fmt.Errorf("%s: %w", f, err)
				}
				if d.Files == nil {
					d.Files = map[string]string{}
				}
				d.Files[path] = string(b)
			}
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
