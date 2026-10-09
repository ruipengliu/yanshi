package sim

import (
	"context"
	"fmt"
)

// simReleases 是模拟中轮换的发布配置：灰度、撤回灰度版本（回滚）、全量后撤回旧版本。
var simReleases = []string{
	`sim: {stable: "1", canary: {version: "2", percent: 50}}`,
	`sim: {stable: "1", withdrawn: ["2"]}`,
	`sim: {stable: "2", withdrawn: ["1"]}`,
}

func (w *World) setRelease(i int) error {
	rel, err := w.agents.ParseReleases([]byte(simReleases[i]))
	if err != nil {
		return err
	}
	w.agents.SetReleases(rel)
	w.tracef("release %d", i)
	return nil
}

// checkNewRunVersion 检查刚开始的 Run 所在 Session 的当前版本没有被撤回：撤回的版本只能服务
// 撤回之前已开始的 Run（ADR-0020）。
func (w *World) checkNewRunVersion(sid string) error {
	st, err := w.svc.Load(context.Background(), sid)
	if err != nil {
		return err
	}
	if _, withdrawn := w.agents.Withdrawn(st.Agent); withdrawn {
		return fmt.Errorf("invariant: new run in %s started on withdrawn version %s", sid, st.Agent.GetVersion())
	}
	return nil
}
