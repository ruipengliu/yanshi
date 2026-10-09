// Package eval 运行评测用例集并与基线比较（docs/design/m4-eval.md，ADR-0018）。
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Case 是一个评测用例（evals/*.yaml）。
type Case struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Agent       string `yaml:"agent"`
	Trials      int    `yaml:"trials"`
	// Requires 列出所需的环境，如 "sandbox"；不满足时跳过。
	Requires []string `yaml:"requires"`
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
		} `yaml:"memories"`
		Device *DeviceSetup `yaml:"device"`
	} `yaml:"setup"`
	Turns []Turn `yaml:"turns"`
}

// DeviceSetup 是虚拟设备：只读的文件、需审批的写文件与发消息。
type DeviceSetup struct {
	Label string            `yaml:"label"`
	Files map[string]string `yaml:"files"`
}

type Turn struct {
	Input string `yaml:"input"`
	// Approve 是本轮遇到审批时的决定，默认批准。
	Approve *bool `yaml:"approve"`
	// NewSession 为 true 时本轮在新的 Session 中进行（同一 EndUser）。
	NewSession bool   `yaml:"new_session"`
	Expect     Expect `yaml:"expect"`
}

type Range struct {
	Min *int `yaml:"min"`
	Max *int `yaml:"max"`
}

type MemoryMatch struct {
	Category string `yaml:"category"`
	Contains string `yaml:"contains"`
}

// Expect 是一轮的断言；未写的字段不检查。
type Expect struct {
	Status        string            `yaml:"status"`
	Calls         []string          `yaml:"calls"`
	NoCalls       []string          `yaml:"no_calls"`
	Approvals     *Range            `yaml:"approvals"`
	ReplyContains []string          `yaml:"reply_contains"`
	ReplyMaxChars int               `yaml:"reply_max_chars"`
	Memories      []MemoryMatch     `yaml:"memories"`
	NoMemories    []MemoryMatch     `yaml:"no_memories"`
	DeviceWrites  map[string]string `yaml:"device_writes"`
	DeviceSent    *int              `yaml:"device_sent"`
	// Compacted 要求到本轮结束时 Session 已发生过上下文压缩（压缩类用例据此确认确实测到了压缩）。
	Compacted bool   `yaml:"compacted"`
	Judge     string `yaml:"judge"`
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
		seen[c.Name] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
