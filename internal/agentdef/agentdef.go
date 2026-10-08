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
	return nil
}

type Registry struct {
	defs map[string]map[string]*Def
	// latest 记录每个 name 最后加载的版本（按文件名排序）。
	latest map[string]string
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

// LoadDir 加载 dir 下所有 *.yaml 文件。
func LoadDir(dir string) (*Registry, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	r, _ := NewRegistry()
	for _, f := range files {
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
