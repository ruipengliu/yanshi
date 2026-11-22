package auth

import (
	"fmt"
	"path"
	"slices"
)

type definitionGlobs struct {
	agents   []string
	oneshots []string
}

// DefinitionAccess 由部署的业务线配置构造；未登记的业务线没有可用定义。
type DefinitionAccess struct {
	lines map[string]definitionGlobs
}

// NewDefinitionAccess 校验 jwt 模式下的定义访问配置。
func NewDefinitionAccess(lines []BusinessLine) (*DefinitionAccess, error) {
	access := &DefinitionAccess{lines: make(map[string]definitionGlobs, len(lines))}
	for _, line := range lines {
		if line.Agents.Allow == nil {
			return nil, fmt.Errorf("auth: business line %q: agents.allow is required", line.Name)
		}
		for _, list := range []struct {
			field string
			globs []string
		}{
			{"agents.allow", line.Agents.Allow},
			{"oneshots.allow", line.Oneshots.Allow},
		} {
			for _, glob := range list.globs {
				if _, err := path.Match(glob, ""); err != nil {
					return nil, fmt.Errorf("auth: business line %q: %s: invalid glob %q: %w", line.Name, list.field, glob, err)
				}
			}
		}
		access.lines[line.Name] = definitionGlobs{agents: slices.Clone(line.Agents.Allow), oneshots: slices.Clone(line.Oneshots.Allow)}
	}
	return access, nil
}

func (a *DefinitionAccess) AllowsAgent(businessLine, name string) bool {
	return matchesDefinition(a.lines[businessLine].agents, name)
}

func (a *DefinitionAccess) AllowsOneshot(businessLine, name string) bool {
	return matchesDefinition(a.lines[businessLine].oneshots, name)
}

func matchesDefinition(globs []string, name string) bool {
	for _, glob := range globs {
		if ok, _ := path.Match(glob, name); ok {
			return true
		}
	}
	return false
}
