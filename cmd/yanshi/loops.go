package main

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"

	"yanshi/internal/agentdef"
	"yanshi/internal/auth"
	"yanshi/internal/extloop"
)

// checkLoopAgents 在启动时核对由外部循环驱动的 AgentDef 与循环登记（ADR-0062 第 7 条）。循环按 Session 的业务线
// 解析，缺登记时要到 Run 执行才以配置错误失败，所以在启动时查出：
//   - AgentDef 指向的循环在任何业务线都没有登记：它永远跑不起来，启动失败；
//   - 业务线的 agents.allow 按名称列出了这个 AgentDef、却没有登记它指向的循环：启动失败；
//   - 只经通配（如开发配置的 "*"）允许、没有登记：只记警告。通配不表示这条业务线打算用它，AgentDef 的名称是全局的，
//     同一个 AgentDef 在不同业务线解析到各自登记的服务。
//   - AgentDef 声明了 loop.models、而它指向的某个登记没有步令牌密钥：启动失败。那条业务线上的外部服务拿不到步令牌，
//     无法经内核的模型端点调用模型（ADR-0063）。
//
// -auth none 时没有业务线配置（lines 为空），只做第一项。
func checkLoopAgents(lines []auth.BusinessLine, agents *agentdef.Registry, loops []extloop.Server, logger *slog.Logger) error {
	type key struct{ businessLine, name string }
	registered := map[key]bool{}
	anywhere := map[string]bool{}
	var keyless []extloop.Server
	for _, s := range loops {
		registered[key{s.BusinessLine, s.Name}] = true
		anywhere[s.Name] = true
		if len(s.TokenKeys) == 0 {
			keyless = append(keyless, s)
		}
	}
	var errs []error
	for _, d := range agents.All() {
		if d.Loop == nil {
			continue
		}
		server := d.Loop.Server
		if !anywhere[server] {
			errs = append(errs, fmt.Errorf("agent %s@%s: loop %q is not registered for any business line", d.Name, d.Version, server))
			continue
		}
		if len(d.Loop.Models) > 0 {
			for _, s := range keyless {
				if s.Name == server {
					errs = append(errs, fmt.Errorf("agent %s@%s declares loop.models, but loop %q of business line %q has no token_keys",
						d.Name, d.Version, server, s.BusinessLine))
				}
			}
		}
		for _, line := range lines {
			if registered[key{line.Name, server}] {
				continue
			}
			if slices.Contains(line.Agents.Allow, d.Name) {
				errs = append(errs, fmt.Errorf("business line %q allows agent %s@%s, but loop %q is not registered for it", line.Name, d.Name, d.Version, server))
				continue
			}
			if slices.ContainsFunc(line.Agents.Allow, func(glob string) bool { ok, _ := path.Match(glob, d.Name); return ok }) {
				logger.Warn("loop agent allowed by a glob but its loop is not registered for the business line; its runs there will fail",
					"agent", d.Name, "version", d.Version, "business_line", line.Name, "loop", server)
			}
		}
	}
	return errors.Join(errs...)
}
