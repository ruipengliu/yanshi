package app

import (
	"context"

	"yanshi/internal/artifact"
	"yanshi/internal/lifecycle"
	"yanshi/internal/sandbox"
)

// SessionData 是除日志、快照与索引之外存放 Session 数据的全部地方（ADR-0015），按 Janitor 清除的顺序：先撤回待投递的
// 调用、移出工作队列，使之后不再有人写入，再清除其余数据。新增一处存放 Session 数据的地方时在这里登记，写入入口
// 经 lifecycle.Recheck 复查删除记录，并在 internal/sim 的删除不变量中检查（docs/design/m2-session-lifecycle.md）。
// 快照在日志删除前后由 Janitor 另行删除，这里同样登记，使第二遍清扫兜住删除期间写入的快照。
func SessionData(s Stores, workspaces sandbox.Workspaces, artifacts *artifact.Service) []lifecycle.Holder {
	return []lifecycle.Holder{
		{Name: "inbox", DeleteSession: s.Inbox.RemoveSession},
		{Name: "work_queue", DeleteSession: s.Queue.Remove},
		{Name: "sandbox", DeleteSession: workspaces.DeleteSession},
		{Name: "sandbox_ledger", DeleteSession: func(_ context.Context, sid string) error { return s.Ledger.Forget(sid) }},
		{Name: "snapshots", DeleteSession: s.Snapshots.Delete},
		// 按保留期到期删除 Session 时保留它写入的 Memory（ADR-0037，docs/design/m4-memory-grant.md §7）。
		{Name: "memories", DeleteSession: s.Memories.DeleteSession, KeepOnRetention: true},
		{Name: "presence", DeleteSession: s.Presence.DeleteSession},
		{Name: "artifacts", DeleteSession: artifacts.DeleteSession},
	}
}
