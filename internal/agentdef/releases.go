package agentdef

import (
	"context"
	"log/slog"
	"slices"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/release"
)

type Release = release.Release
type Canary = release.Canary
type Releases = release.Releases

func (r *Registry) ParseReleases(b []byte) (Releases, error) {
	return release.Parse(b, func(name, version string) bool { _, ok := r.defs[name][version]; return ok })
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
	v := release.Select(name, endUser, x)
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

func inCanary(name, endUser string, c *Canary) bool { return release.InCanary(name, endUser, c) }

func (r *Registry) LoadReleases(path string) (Releases, []byte, error) {
	return release.Load(path, r.ParseReleases)
}

func (r *Registry) WatchReleases(ctx context.Context, path string, loaded []byte, every time.Duration, logger *slog.Logger) {
	release.Watch(ctx, path, loaded, every, logger, r.ParseReleases, r.SetReleases)
}

// Label 是指标与用量中标识版本的字符串 name@version。
func Label(ref *v1.AgentRef) string { return ref.GetName() + "@" + ref.GetVersion() }
