// Package agentdef 加载版本化的 AgentDef。
//
// AgentDef 是业务线声明 Agent 的方式：YAML 文件，每个文件一个 (name, version)。
// Session 创建时固定 AgentDef 版本，之后的 Run 都按该版本执行。
package agentdef

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	v1 "yanshi/gen/yanshi/v1"
)

type Def struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	// Instructions 是系统指令。
	Instructions string `yaml:"instructions"`
	// Model 是 "provider/model" 形式的模型引用。
	Model        string   `yaml:"model"`
	Capabilities []string `yaml:"capabilities"`
	// MaxTurns 限制一个 Run 内模型调用的次数。
	MaxTurns int `yaml:"max_turns"`
	// Context 控制上下文窗口与压缩（docs/design/m2-long-runs.md §4）。
	Context Context `yaml:"context"`
	// Memory 控制 Run 开始时的召回（docs/design/m4-memory-grant.md §3）。能否写入由能力白名单中
	// 是否包含 memory_save 决定。
	Memory MemoryConfig `yaml:"memory"`
	// Call 配置 Session 中的 Call（全双工语音通话，docs/design/m3-call.md）；Model 为空表示不支持通话。
	Call CallConfig `yaml:"call"`
}

// CallConfig 是通话中语音模型的配置。语音模型只负责对话；需要能力的工作经 run_task 交给本 AgentDef 的 Run 执行。
type CallConfig struct {
	// Model 是实时语音模型引用，如 "volc/1.2.6.1"。
	Model string `yaml:"model"`
	// Voice 是音色 ID，留空使用提供商的默认音色。
	Voice string `yaml:"voice"`
	// Instructions 是语音模型的系统指令（人设、说话风格）；run_task 的用法由工具说明给出。
	Instructions string `yaml:"instructions"`
}

type MemoryConfig struct {
	// Recall 是每个 Run 开始时召回的条数；0 表示不召回。
	Recall int `yaml:"recall"`
}

// Context 是上下文压缩策略的参数，单位均为（估算的）token。
type Context struct {
	// Window 是模型的上下文窗口。
	Window int `yaml:"window"`
	// CompactAt 是触发压缩的比例：估算请求大小超过 Window × CompactAt 时压缩。
	CompactAt float64 `yaml:"compact_at"`
	// KeepRecent 是压缩后保留原文的历史量。
	KeepRecent int `yaml:"keep_recent"`
	// MaxToolResult 是单条调用结果在上下文中的上限，超出部分截断（日志中保留全文）。
	MaxToolResult int `yaml:"max_tool_result"`
	// SummaryModel 是生成摘要的模型引用，留空表示与 Def.Model 相同。
	SummaryModel string `yaml:"summary_model"`
}

// Threshold 是触发压缩的请求大小。
func (c Context) Threshold() int { return int(float64(c.Window) * c.CompactAt) }

func (c *Context) validate() error {
	if c.Window == 0 {
		c.Window = 64000
	}
	if c.CompactAt == 0 {
		c.CompactAt = 0.75
	}
	if c.KeepRecent == 0 {
		c.KeepRecent = c.Window / 8
	}
	if c.MaxToolResult == 0 {
		c.MaxToolResult = c.Window / 8
	}
	// 保留量与单条结果都必须明显小于阈值，否则压缩后仍可能立即超限、无法收敛。
	switch {
	case c.Window < 0 || c.CompactAt <= 0 || c.CompactAt > 0.95:
		return fmt.Errorf("context: window must be positive and compact_at in (0, 0.95]")
	case c.KeepRecent < 0 || c.KeepRecent > c.Threshold()/2:
		return fmt.Errorf("context: keep_recent must be at most half of window × compact_at")
	case c.MaxToolResult < 0 || c.MaxToolResult > c.Window/4:
		return fmt.Errorf("context: max_tool_result must be at most a quarter of window")
	}
	return nil
}

func (d *Def) Ref() *v1.AgentRef { return &v1.AgentRef{Name: d.Name, Version: d.Version} }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func (d *Def) validate() error {
	if !nameRE.MatchString(d.Name) || d.Version == "" || d.Model == "" {
		return fmt.Errorf("agent %q@%q: name, version and model are required", d.Name, d.Version)
	}
	if d.MaxTurns <= 0 {
		d.MaxTurns = 16
	}
	if err := d.Context.validate(); err != nil {
		return fmt.Errorf("agent %s@%s: %w", d.Name, d.Version, err)
	}
	return nil
}

type Registry struct {
	defs map[string]map[string]*Def
	// latest 记录每个 name 最后加载的版本（按文件名排序），是没有发布配置时的默认版本。
	latest map[string]string
	// releases 是发布配置（灰度与撤回），运行中可替换。
	releases atomic.Pointer[Releases]
}

func NewRegistry(defs ...*Def) (*Registry, error) {
	r := &Registry{defs: map[string]map[string]*Def{}, latest: map[string]string{}}
	for _, d := range defs {
		if err := r.add(d); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) add(d *Def) error {
	if err := d.validate(); err != nil {
		return err
	}
	if r.defs[d.Name] == nil {
		r.defs[d.Name] = map[string]*Def{}
	}
	if _, dup := r.defs[d.Name][d.Version]; dup {
		return fmt.Errorf("agent %s@%s defined twice", d.Name, d.Version)
	}
	r.defs[d.Name][d.Version] = d
	r.latest[d.Name] = d.Version
	return nil
}

// ReleasesFile 是发布配置在 AgentDef 目录中的文件名；LoadDir 跳过它。
const ReleasesFile = "releases.yaml"

// LoadDir 加载 dir 下所有 *.yaml 文件（发布配置除外）。
func LoadDir(dir string) (*Registry, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	r, _ := NewRegistry()
	for _, f := range files {
		if filepath.Base(f) == ReleasesFile {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		d := &Def{}
		if err := yaml.Unmarshal(b, d); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := r.add(d); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return r, nil
}

func (r *Registry) Get(ref *v1.AgentRef) (*Def, error) {
	d, ok := r.defs[ref.GetName()][ref.GetVersion()]
	if !ok {
		return nil, fmt.Errorf("agent %s@%s not found", ref.GetName(), ref.GetVersion())
	}
	return d, nil
}

// Latest 返回 name 的默认版本。
func (r *Registry) Latest(name string) (*Def, error) {
	v, ok := r.latest[name]
	if !ok {
		return nil, fmt.Errorf("agent %q not found", name)
	}
	return r.defs[name][v], nil
}

// All 返回全部已加载的 AgentDef（按名称、版本排序）。
func (r *Registry) All() []*Def {
	var out []*Def
	for _, versions := range r.defs {
		for _, d := range versions {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}
