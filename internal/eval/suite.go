// Package eval 运行评测用例集并与基线比较（docs/design/m4-eval.md，ADR-0018）。
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"yanshi/internal/xinlimock"
)

// Case 是一个评测用例（evals/*.yaml）。
type Case struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Agent       string `yaml:"agent"`
	Trials      int    `yaml:"trials"`
	// Timeout 是每一轮等待 Run 结束的上限，默认取 Config.TurnTimeout（长 Run 用例需要更长）。
	Timeout time.Duration `yaml:"timeout"`
	// Requires 列出所需的环境，如 "sandbox"；不满足时跳过。
	Requires []string `yaml:"requires"`
	// Capabilities 追加到被评测 AgentDef 的能力白名单（如 "mcp:crm/*"）。
	Capabilities []string `yaml:"capabilities"`
	// Context 覆盖 AgentDef 的上下文配置（零值字段不覆盖）。
	Context struct {
		Window        int `yaml:"window"`
		KeepRecent    int `yaml:"keep_recent"`
		MaxToolResult int `yaml:"max_tool_result"`
	} `yaml:"context"`
	// Call 为 true 时用例在一次 Call 中进行（docs/design/m3-call.md §9）：每轮的 input 合成为语音说出，
	// 由实时语音模型应答、经 run_task 派生任务。需要实时语音模型与语音合成，不满足时跳过。
	Call  bool `yaml:"call"`
	Setup struct {
		Memories []struct {
			Category string `yaml:"category"`
			Content  string `yaml:"content"`
			// Recorded 是记录日期（YYYY-MM-DD），用于预置新旧不同的记忆；默认为当前时间。
			Recorded string `yaml:"recorded"`
		} `yaml:"memories"`
		Device *DeviceSetup `yaml:"device"`
		// Devices 是另外几台设备（标签互不相同），用于检验同类能力有多台设备时的选择（docs/design/m6-decisions.md §3）。
		Devices []DeviceSetup `yaml:"devices"`
		// CRM 为 true 时为评测业务线登记内置的 CRM MCP Server（工具 search_customer、create_ticket）。
		CRM bool `yaml:"crm"`
		// Agents 是本用例可委托的 Agent（docs/design/m5-multi-agent.md）：与被评测的 Agent 使用同一模型；
		// 名称在整个评测集中唯一，被评测的 Agent 须在 capabilities 中以 "agent:<名称>" 允许委托。
		Agents []AgentSetup `yaml:"agents"`
		// Xinli 非 nil 时为评测业务线登记内置的 xinli MCP Server（docs/design/captures.md §6），并为本次运行的 EndUser
		// 预置其中的项目与每日摘要。
		Xinli *XinliSetup `yaml:"xinli"`
		// NoHealth 为 true 时本用例在未开启健康信息的业务线中运行（ADR-0022）：health 类别的写入被拒绝。
		NoHealth bool `yaml:"no_health"`
	} `yaml:"setup"`
	Turns []Turn `yaml:"turns"`
}

// AgentSetup 是评测用的被委托 Agent。
type AgentSetup struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Instructions string   `yaml:"instructions"`
	Capabilities []string `yaml:"capabilities"`
}

// XinliSetup 是 xinli MCP Server 中预置的数据：已有的项目与按日期的摘要（file_day 交给业务线、day_summary 取回的内容）。
type XinliSetup struct {
	Projects []XinliProject    `yaml:"projects"`
	Days     map[string]string `yaml:"days"`
}

type XinliProject = xinlimock.Project

// CaptureSegment 是一段旁听转写：Speaker 为 wearer（佩戴者）或 other（他人，Key 区分不同的人）。
type CaptureSegment struct {
	Speaker string `yaml:"speaker"`
	Key     string `yaml:"key"`
	Text    string `yaml:"text"`
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
	Input string `yaml:"input"`
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
	// NewSession 为 true 时本轮在新的 Session 中进行（同一 EndUser）。
	NewSession bool `yaml:"new_session"`
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
	// Captures 是本轮输入之前由 HostApp 提交到本轮 Session 的一批旁听（docs/design/captures.md §3）；
	// 跨天的用例在 new_session 的一轮提交新一天的旁听。
	Captures []CaptureSegment `yaml:"captures"`
	Expect   Expect           `yaml:"expect"`
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
}

// Expect 是一轮的断言；未写的字段不检查。
type Expect struct {
	// Status 是本轮结束时 Run 的状态，可用 "|" 列出多个可接受的值，如 "completed|asked"
	// （asked：停在 ask_user 提问处等待回答）。默认 completed。
	Status  string   `yaml:"status"`
	Calls   []string `yaml:"calls"`
	NoCalls []string `yaml:"no_calls"`
	// MaxCalls 限制名称匹配（glob）的调用在本轮出现的次数，如 {"macbook__read_file": 1}：调用转入后台后不应重试。
	MaxCalls  map[string]int `yaml:"max_calls"`
	Approvals *Range         `yaml:"approvals"`
	// Questions 是本轮 ask_user 提问次数的范围。
	Questions     *Range            `yaml:"questions"`
	ReplyContains []string          `yaml:"reply_contains"`
	ReplyMaxChars int               `yaml:"reply_max_chars"`
	Memories      []MemoryMatch     `yaml:"memories"`
	NoMemories    []MemoryMatch     `yaml:"no_memories"`
	DeviceWrites  map[string]string `yaml:"device_writes"`
	DeviceSent    *int              `yaml:"device_sent"`
	// CallArgs 要求某个名称匹配（glob）的调用的参数包含给定文本，如 {"*send_message": "lilei@example.com"}。
	CallArgs map[string]string `yaml:"call_args"`
	// NoCallArgs 要求名称匹配（glob）的调用的参数都不包含给定文本，如 {"mcp_xinli__set_commitments": "合同"}。
	NoCallArgs map[string]string `yaml:"no_call_args"`
	// Compacted 要求到本轮结束时 Session 已发生过上下文压缩（压缩类用例据此确认确实测到了压缩）。
	Compacted bool `yaml:"compacted"`
	// Compactions 与 Takeovers 是本轮 Run 内上下文压缩与接管次数的范围（长 Run 用例）。
	Compactions *Range `yaml:"compactions"`
	Takeovers   *Range `yaml:"takeovers"`
	// Tasks 是本轮 Call 中派生任务（run_task）次数的范围；TaskContains 要求某个任务的内容包含给定文本。
	Tasks        *Range   `yaml:"tasks"`
	TaskContains []string `yaml:"task_contains"`
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
		if !caseNameRE.MatchString(c.Name) || seen[c.Name] {
			return nil, fmt.Errorf("%s: case name %q missing, invalid or duplicated", f, c.Name)
		}
		if len(c.Turns) == 0 {
			return nil, fmt.Errorf("%s: no turns", f)
		}
		for i, t := range c.Turns {
			if t.AwaitBackground && (t.Input != "" || t.CardAction != nil || t.NewSession || c.Call) {
				return nil, fmt.Errorf("%s: turn %d awaits a background result and cannot also submit input", f, i+1)
			}
			for _, g := range t.Captures {
				if g.Speaker != "wearer" && g.Speaker != "other" {
					return nil, fmt.Errorf("%s: turn %d: capture speaker %q must be wearer or other", f, i+1, g.Speaker)
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
		if c.Agent == "" {
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
