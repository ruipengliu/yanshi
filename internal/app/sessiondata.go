package app

import (
	"context"
	"errors"

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
		{Name: "inbox", DeleteSessionForEndUser: s.Inbox.RemoveSession, DeleteSession: func(ctx context.Context, sid string) error {
			// 索引在两遍清扫后才删除；删除后重试则从 Inbox 离线恢复归属，不能靠槽位猜 EndUser。
			entry, err := s.Index.LookupSession(ctx, sid)
			if errors.Is(err, lifecycle.ErrNotFound) {
				return s.Inbox.RemoveSession(ctx, "", sid)
			}
			if err != nil {
				return err
			}
			return s.Inbox.RemoveSession(ctx, entry.EndUser, sid)
		}},
		// 包括 PostgreSQL 的 work_dues；Remove 在同一事务中清除两张表。
		{Name: "work_queue", DeleteSession: s.Queue.Remove},
		{Name: "sandbox", DeleteSession: workspaces.DeleteSession},
		{Name: "sandbox_ledger", DeleteSession: func(_ context.Context, sid string) error { return s.Ledger.Forget(sid) }},
		{Name: "snapshots", DeleteSession: s.Snapshots.Delete},
		{Name: "outbox", DeleteSession: s.Outbox.DeleteSession},
		// retention 与 erasure 保留既有 Memory；迟到写入仍由 Recheck 撤回（ADR-0050，docs/design/m4-memory-grant.md §7）。
		{Name: "memories", DeleteSession: s.Memories.DeleteSession, DeleteSessionForEndUser: s.Memories.DeleteSessionForEndUser, KeepOnRetention: true},
		{Name: "presence", DeleteSession: s.Presence.DeleteSession},
		{Name: "artifacts", DeleteSession: artifacts.DeleteSession},
	}
}
