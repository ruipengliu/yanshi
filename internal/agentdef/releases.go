package agentdef

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/metrics"
)

// Release 是一个 Agent 的发布状态（agents/releases.yaml，docs/design/m4-agent-rollout.md §2）。
type Release struct {
	Stable    string   `yaml:"stable"`
	Canary    *Canary  `yaml:"canary"`
	Withdrawn []string `yaml:"withdrawn"`
}

// Canary 是灰度版本：分给 Percent% 的 EndUser，EndUsers 中的用户总是进入灰度。
type Canary struct {
	Version  string   `yaml:"version"`
	Percent  float64  `yaml:"percent"`
	EndUsers []string `yaml:"end_users"`
}

// Releases 以 Agent 名为键。
type Releases map[string]Release

// ParseReleases 解析并按 r 中已加载的 AgentDef 校验发布配置。
func (r *Registry) ParseReleases(b []byte) (Releases, error) {
	rel := Releases{}
	if err := yaml.Unmarshal(b, &rel); err != nil {
		return nil, err
	}
	for name, x := range rel {
		has := func(v string) bool { _, ok := r.defs[name][v]; return ok }
		if x.Stable == "" || !has(x.Stable) {
			return nil, fmt.Errorf("releases: %s: stable version %q is not loaded", name, x.Stable)
		}
		if slices.Contains(x.Withdrawn, x.Stable) {
			return nil, fmt.Errorf("releases: %s: stable version %q is withdrawn", name, x.Stable)
		}
		if c := x.Canary; c != nil {
			switch {
			case !has(c.Version):
				return nil, fmt.Errorf("releases: %s: canary version %q is not loaded", name, c.Version)
			case slices.Contains(x.Withdrawn, c.Version):
				return nil, fmt.Errorf("releases: %s: canary version %q is withdrawn", name, c.Version)
			case c.Percent < 0 || c.Percent > 100:
				return nil, fmt.Errorf("releases: %s: canary percent must be in [0, 100]", name)
			}
		}
	}
	return rel, nil
}

// SetReleases 替换发布配置（调用方已用 ParseReleases 校验）。并发安全：创建 Session 与重新加载可以同时进行。
func (r *Registry) SetReleases(rel Releases) { r.releases.Store(&rel) }

func (r *Registry) release(name string) (Release, bool) {
	p := r.releases.Load()
	if p == nil {
		return Release{}, false
	}
	x, ok := (*p)[name]
	return x, ok
}

// inCanary 按 EndUser 稳定分流：散列包含 Agent 名，不同 Agent 的灰度人群互不相关；
// 比例调大时阈值变大，已在灰度中的用户仍在其中。
func inCanary(name, endUser string, c *Canary) bool {
	if c == nil {
		return false
	}
	if slices.Contains(c.EndUsers, endUser) {
		return true
	}
	h := fnv.New64a()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(endUser))
	return h.Sum64()%10000 < uint64(c.Percent*100)
}

// Default 返回 name 的默认版本：有发布配置时为稳定版本，否则为最后加载的版本。
func (r *Registry) Default(name string) (*Def, error) {
	if x, ok := r.release(name); ok {
		return r.Get(&v1.AgentRef{Name: name, Version: x.Stable})
	}
	return r.Latest(name)
}

// Resolve 返回 EndUser 新建 Session 时使用的版本：有发布配置时按灰度分流，否则为默认版本。
func (r *Registry) Resolve(name, endUser string) (*Def, error) {
	x, ok := r.release(name)
	if !ok {
		return r.Latest(name)
	}
	v := x.Stable
	if inCanary(name, endUser, x.Canary) {
		v = x.Canary.Version
	}
	return r.Get(&v1.AgentRef{Name: name, Version: v})
}

// Withdrawn 报告 ref 是否已撤回；是则返回应切换到的稳定版本。
func (r *Registry) Withdrawn(ref *v1.AgentRef) (*Def, bool) {
	x, ok := r.release(ref.GetName())
	if !ok || !slices.Contains(x.Withdrawn, ref.GetVersion()) {
		return nil, false
	}
	d, err := r.Get(&v1.AgentRef{Name: ref.GetName(), Version: x.Stable})
	return d, err == nil
}

// LoadReleases 读取发布配置文件；文件不存在时返回空配置。
func (r *Registry) LoadReleases(path string) (Releases, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Releases{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rel, err := r.ParseReleases(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return rel, b, nil
}

// WatchReleases 每隔 every 检查一次发布配置文件，内容变化时校验并替换；校验失败时保留原配置
// （回滚不需要重启，docs/design/m4-agent-rollout.md §2）。loaded 是启动时已加载的文件内容。
func (r *Registry) WatchReleases(ctx context.Context, path string, loaded []byte, every time.Duration, logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			b, err = nil, nil
		}
		if err != nil || bytes.Equal(b, loaded) {
			continue
		}
		rel, err := r.ParseReleases(b)
		if err != nil {
			metrics.ReleaseReloadErrors.WithLabelValues().Inc()
			logger.Error("releases reload rejected; keeping the previous configuration", "path", path, "err", err)
			loaded = b // 同一份错误内容不重复报错
			continue
		}
		r.SetReleases(rel)
		loaded = b
		logger.Info("releases reloaded", "path", path, "agents", len(rel))
	}
}

// Label 是指标与用量中标识版本的字符串 name@version。
func Label(ref *v1.AgentRef) string { return ref.GetName() + "@" + ref.GetVersion() }
