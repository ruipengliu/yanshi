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
	"path/filepath"
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
	rel, loaded, err := (Files{{Path: path, Parse: parse}}).Load()
	if err != nil {
		return nil, nil, err
	}
	return rel, loaded[0], nil
}

// Watch 每隔 every 检查一次发布配置文件，内容变化时校验并替换；校验失败时保留原配置
// （回滚不需要重启，docs/design/m4-agent-rollout.md §2）。loaded 是启动时已加载的文件内容。
func Watch(ctx context.Context, path string, loaded []byte, every time.Duration, logger *slog.Logger, parse func([]byte) (Releases, error), set func(Releases)) {
	(Files{{Path: path, Parse: parse}}).Watch(ctx, [][]byte{loaded}, every, logger, set)
}

// File 用同一目录的定义校验发布配置，避免发布条目跨目录引用定义。
type File struct {
	Path  string
	Parse func([]byte) (Releases, error)
}

// Files 合并来自不同定义目录的发布配置，目录之间的定义名已去重。
type Files []File

// Load 读取并合并整组发布配置，返回供 Watch 比较的文件内容。
func (files Files) Load() (Releases, [][]byte, error) {
	contents, err := files.read()
	if err != nil {
		return nil, nil, err
	}
	rel, err := files.parse(contents)
	if err != nil {
		return nil, nil, err
	}
	return rel, contents, nil
}

func (files Files) read() ([][]byte, error) {
	contents := make([][]byte, len(files))
	for i, file := range files {
		b, err := os.ReadFile(file.Path)
		if errors.Is(err, os.ErrNotExist) {
			// 多目录部署中，挂载丢失时保留发布选择，单个发布文件移除时恢复默认选择。
			err = nil
			if len(files) > 1 {
				_, err = os.ReadDir(filepath.Dir(file.Path))
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.Path, err)
		}
		contents[i] = b
	}
	return contents, nil
}

func (files Files) parse(contents [][]byte) (Releases, error) {
	merged := Releases{}
	for i, file := range files {
		if contents[i] == nil {
			continue
		}
		rel, err := file.Parse(contents[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.Path, err)
		}
		for name, entry := range rel {
			merged[name] = entry
		}
	}
	return merged, nil
}

// Watch 只在所有发布配置均可读取并通过校验后替换整组选择。
func (files Files) Watch(ctx context.Context, loaded [][]byte, every time.Duration, logger *slog.Logger, set func(Releases)) {
	if len(files) == 1 {
		logger = logger.With("path", files[0].Path)
	} else {
		paths := make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.Path)
		}
		logger = logger.With("paths", paths)
	}
	var lastReadError string
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		contents, err := files.read()
		if err != nil {
			if message := err.Error(); len(files) > 1 && message != lastReadError {
				lastReadError = message
				metrics.ReleaseReloadErrors.WithLabelValues().Inc()
				logger.Error("releases reload rejected; keeping the previous configuration", "err", err)
			}
			continue
		}
		lastReadError = ""
		if slices.EqualFunc(contents, loaded, bytes.Equal) {
			continue
		}
		rel, err := files.parse(contents)
		loaded = contents // 同一份错误内容不重复报错
		if err != nil {
			metrics.ReleaseReloadErrors.WithLabelValues().Inc()
			logger.Error("releases reload rejected; keeping the previous configuration", "err", err)
			continue
		}
		set(rel)
		logger.Info("releases reloaded", "declarations", len(rel))
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
