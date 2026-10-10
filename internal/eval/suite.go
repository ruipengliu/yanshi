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
	Setup struct {
		Memories []struct {
			Category string `yaml:"category"`
			Content  string `yaml:"content"`
			// Recorded 是记录日期（YYYY-MM-DD），用于预置新旧不同的记忆；默认为当前时间。
			Recorded string `yaml:"recorded"`
		} `yaml:"memories"`
		Device *DeviceSetup `yaml:"device"`
		// CRM 为 true 时为评测业务线登记内置的 CRM MCP Server（工具 search_customer、create_ticket）。
		CRM bool `yaml:"crm"`
	} `yaml:"setup"`
	Turns []Turn `yaml:"turns"`
}

// DeviceSetup 是虚拟设备：只读的文件、需审批的写文件与发消息。
type DeviceSetup struct {
	Label string            `yaml:"label"`
	Files map[string]string `yaml:"files"`
	// FilesFrom 从评测集目录下的文件读取设备文件内容（大文件），键为设备上的路径。
	FilesFrom map[string]string `yaml:"files_from"`
	// OnlineAfter > 0 时设备在本次运行开始后这么久才上线：检验设备离线时 Run 挂起、上线后恢复。
	OnlineAfter time.Duration `yaml:"online_after"`
}

type Turn struct {
	Input string `yaml:"input"`
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
	// Answer 是本轮遇到 ask_user 提问时的回答方式；为空时本轮停在提问处（状态 asked），
	// 下一轮的输入即作为回答（与用户直接打字回答一致）。
	Answer *AnswerScript `yaml:"answer"`
	Expect Expect        `yaml:"expect"`
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
	Status    string   `yaml:"status"`
	Calls     []string `yaml:"calls"`
	NoCalls   []string `yaml:"no_calls"`
	Approvals *Range   `yaml:"approvals"`
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
	// Compacted 要求到本轮结束时 Session 已发生过上下文压缩（压缩类用例据此确认确实测到了压缩）。
	Compacted bool `yaml:"compacted"`
	// Compactions 与 Takeovers 是本轮 Run 内上下文压缩与接管次数的范围（长 Run 用例）。
	Compactions *Range `yaml:"compactions"`
	Takeovers   *Range `yaml:"takeovers"`
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
	seen := map[string]bool{}
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
