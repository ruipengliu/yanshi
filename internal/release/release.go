// Package release 复用声明的发布校验与按 EndUser 的稳定分流，不包含 Session 语义。
package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"os"
	"regexp"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"yanshi/internal/metrics"
)

// Release 是一个声明的发布状态（docs/design/m4-agent-rollout.md）。
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

// Releases 以声明名为键。
type Releases map[string]Release

// Parse 按已加载的声明校验发布配置。
func Parse(b []byte, hasVersion func(name, version string) bool) (Releases, error) {
	rel := Releases{}
	if err := yaml.Unmarshal(b, &rel); err != nil {
		return nil, err
	}
	for name, x := range rel {
		has := func(v string) bool { return hasVersion(name, v) }
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
			case math.IsNaN(c.Percent) || math.IsInf(c.Percent, 0) || c.Percent < 0 || c.Percent > 100:
				return nil, fmt.Errorf("releases: %s: canary percent must be in [0, 100]", name)
			}
		}
	}
	return rel, nil
}

// InCanary 按 EndUser 稳定分流：散列包含声明名，不同声明的灰度人群互不相关；
// 比例调大时阈值变大，已在灰度中的用户仍在其中。
func InCanary(name, endUser string, c *Canary) bool {
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

// Load 读取发布配置文件；文件不存在时返回空配置。
func Load(path string, parse func([]byte) (Releases, error)) (Releases, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Releases{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rel, err := parse(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return rel, b, nil
}

// Watch 每隔 every 检查一次发布配置文件，内容变化时校验并替换；校验失败时保留原配置
// （回滚不需要重启，docs/design/m4-agent-rollout.md §2）。loaded 是启动时已加载的文件内容。
func Watch(ctx context.Context, path string, loaded []byte, every time.Duration, logger *slog.Logger, parse func([]byte) (Releases, error), set func(Releases)) {
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
		rel, err := parse(b)
		if err != nil {
			metrics.ReleaseReloadErrors.WithLabelValues().Inc()
			logger.Error("releases reload rejected; keeping the previous configuration", "path", path, "err", err)
			loaded = b // 同一份错误内容不重复报错
			continue
		}
		set(rel)
		loaded = b
		logger.Info("releases reloaded", "path", path, "declarations", len(rel))
	}
}

// Select 为一次请求固定版本；热加载只影响下一次选择。
func Select(name, endUser string, x Release) string {
	if InCanary(name, endUser, x.Canary) {
		return x.Canary.Version
	}
	return x.Stable
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidIdentity 与 AgentDef 保持同一格式；版本是非空的不透明字符串。
func ValidIdentity(name, version string) bool { return nameRE.MatchString(name) && version != "" }
