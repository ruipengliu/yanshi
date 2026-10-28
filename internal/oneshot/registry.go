// Package oneshot 执行没有 Session、工具与结果存储的声明式单次调用（ADR-0047）。
package oneshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"

	"yanshi/internal/model"
	"yanshi/internal/release"
)

var (
	ErrNotFound = errors.New("oneshot not found")
	ErrInvalid  = errors.New("invalid oneshot input")
	ErrSchema   = errors.New("oneshot output does not match schema")
	ErrProvider = errors.New("oneshot model call failed")
)

type Input struct {
	MaxChars int `yaml:"max_chars"`
}

type Def struct {
	Name            string `yaml:"name"`
	Version         string `yaml:"version"`
	Model           string `yaml:"model"`
	Instructions    string `yaml:"instructions"`
	Input           Input  `yaml:"input"`
	Output          any    `yaml:"output"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`
	Concurrency     int    `yaml:"concurrency"`
	WorkClass       string `yaml:"work_class"`
	// Kind 只兼容显式 generate，避免旧配置的 embedding 被静默当作生成。
	Kind       string `yaml:"kind,omitempty"`
	schema     *jsonschema.Resolved
	schemaJSON string
}

func (d *Def) Label() string   { return d.Name + "@" + d.Version }
func (d *Def) HasSchema() bool { return d.schema != nil }

func (d *Def) validate() error {
	d.schema, d.schemaJSON = nil, ""
	if !release.ValidIdentity(d.Name, d.Version) || strings.TrimSpace(d.Instructions) == "" || d.Input.MaxChars <= 0 || d.MaxOutputTokens <= 0 || d.Concurrency <= 0 {
		return errors.New("oneshot: name, version, instructions and positive input.max_chars, max_output_tokens, concurrency are required")
	}
	if _, _, err := model.SplitRef(d.Model); err != nil {
		return err
	}
	if d.Kind != "" && d.Kind != "generate" {
		return errors.New("oneshot: only generate is supported")
	}
	if d.WorkClass == "" {
		d.WorkClass = "interactive"
	}
	if d.WorkClass != "interactive" && d.WorkClass != "background" {
		return errors.New("oneshot: work_class must be interactive or background")
	}
	if d.Output == nil {
		return nil
	}
	b, err := json.Marshal(d.Output)
	if err != nil {
		return fmt.Errorf("oneshot output schema: %w", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(b, &schema); err != nil {
		return fmt.Errorf("oneshot output schema: %w", err)
	}
	if err := localRefs(&schema); err != nil {
		return err
	}
	// 不提供 Loader：即使漏过前面的校验，也不能发起外部请求。
	d.schema, err = schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("oneshot output schema: %w", err)
	}
	d.schemaJSON = string(b)
	return nil
}

// 只遍历 Schema 子节点检查类型与引用，不把名为 $ref 的业务属性或 examples 中的值误当作引用。
func localRefs(s *jsonschema.Schema) error {
	if s == nil {
		return nil
	}
	// Resolve 不检查 type 的取值，必须在加载时拒绝错误的声明，不能等模型输出后才失败。
	types := s.Types
	if s.Type != "" {
		types = append([]string{s.Type}, types...)
	}
	if s.Types != nil && len(s.Types) == 0 {
		return errors.New("oneshot output schema: type array must not be empty")
	}
	for _, typ := range types {
		switch typ {
		case "null", "boolean", "object", "array", "number", "string", "integer":
		default:
			return fmt.Errorf("oneshot output schema: invalid type %q", typ)
		}
	}
	for _, ref := range []string{s.Ref, s.DynamicRef} {
		if ref != "" && !strings.HasPrefix(ref, "#") {
			return errors.New("oneshot output schema: external references are forbidden")
		}
	}
	v := reflect.ValueOf(s).Elem()
	for i := 0; i < v.NumField(); i++ {
		if !v.Field(i).CanInterface() {
			continue
		}
		switch children := v.Field(i).Interface().(type) {
		case *jsonschema.Schema:
			if err := localRefs(children); err != nil {
				return err
			}
		case []*jsonschema.Schema:
			for _, child := range children {
				if err := localRefs(child); err != nil {
					return err
				}
			}
		case map[string]*jsonschema.Schema:
			for _, child := range children {
				if err := localRefs(child); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ValidateOutput 只返回固定类别，校验库的错误可能带模型原文，不能进入修复提示或日志。
func (d *Def) ValidateOutput(text string) (any, string) {
	if d.schema == nil {
		return text, ""
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, "invalid_json"
	}
	if err := d.schema.Validate(value); err != nil {
		return nil, "schema_mismatch"
	}
	return json.RawMessage(text), ""
}

type Registry struct {
	defs     map[string]map[string]*Def
	latest   map[string]string
	releases atomic.Pointer[release.Releases]
}

func NewRegistry(defs ...*Def) (*Registry, error) {
	r := &Registry{defs: map[string]map[string]*Def{}, latest: map[string]string{}}
	for _, d := range defs {
		if err := d.validate(); err != nil {
			return nil, err
		}
		if r.defs[d.Name] == nil {
			r.defs[d.Name] = map[string]*Def{}
		}
		for _, previous := range r.defs[d.Name] {
			if previous.Concurrency != d.Concurrency {
				return nil, fmt.Errorf("oneshot %s: concurrency must match across versions", d.Name)
			}
		}
		if r.defs[d.Name][d.Version] != nil {
			return nil, fmt.Errorf("oneshot %s defined twice", d.Label())
		}
		r.defs[d.Name][d.Version], r.latest[d.Name] = d, d.Version
	}
	return r, nil
}

const ReleasesFile = "releases.yaml"

func LoadDir(dir string) (*Registry, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var defs []*Def
	for _, path := range files {
		if filepath.Base(path) == ReleasesFile {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		d := &Def{}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(d); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, err
		}
		if len(doc.Content) > 0 {
			fields := doc.Content[0].Content
			for i := 0; i+1 < len(fields); i += 2 {
				if fields[i].Value == "kind" && fields[i+1].Value != "generate" {
					return nil, fmt.Errorf("%s: oneshot: only generate is supported", path)
				}
				if fields[i].Value == "output" && fields[i+1].Tag == "!!null" {
					return nil, fmt.Errorf("%s: output must be a JSON Schema", path)
				}
			}
		}
		defs = append(defs, d)
	}
	return NewRegistry(defs...)
}

func (r *Registry) Get(name, version string) (*Def, error) {
	if r != nil {
		if d := r.defs[name][version]; d != nil {
			return d, nil
		}
	}
	return nil, ErrNotFound
}

func (r *Registry) Resolve(name, endUser string) (*Def, error) {
	if r == nil {
		return nil, ErrNotFound
	}
	version := r.latest[name]
	if rel := r.releases.Load(); rel != nil {
		if x, ok := (*rel)[name]; ok {
			version = release.Select(name, endUser, x)
		}
	}
	return r.Get(name, version)
}

func (r *Registry) All() []*Def {
	var out []*Def
	if r == nil {
		return out
	}
	for _, versions := range r.defs {
		for _, d := range versions {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label() < out[j].Label() })
	return out
}

func (r *Registry) ParseReleases(b []byte) (release.Releases, error) {
	return release.Parse(b, func(name, version string) bool { _, err := r.Get(name, version); return err == nil })
}
func (r *Registry) SetReleases(rel release.Releases) { r.releases.Store(&rel) }
func (r *Registry) LoadReleases(path string) (release.Releases, []byte, error) {
	return release.Load(path, r.ParseReleases)
}
func (r *Registry) WatchReleases(ctx context.Context, path string, loaded []byte, every time.Duration, logger *slog.Logger) {
	release.Watch(ctx, path, loaded, every, logger, r.ParseReleases, r.SetReleases)
}

// Default 供发布前评测选择稳定版本，不把评测 EndUser 分入线上灰度。
func (r *Registry) Default(name string) (*Def, error) {
	if r == nil {
		return nil, ErrNotFound
	}
	version := r.latest[name]
	if rel := r.releases.Load(); rel != nil {
		if x, ok := (*rel)[name]; ok {
			version = x.Stable
		}
	}
	return r.Get(name, version)
}
