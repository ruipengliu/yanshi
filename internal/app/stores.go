// Package app 是装配：一整套存储实现由这里构造，serve、模拟测试、评测与 e2e 测试都经它取得存储
// （延展性评审 §9.1）。新增一种存储实现（如 ADR-0028 的分片层）只改这里与存储包本身。
package app

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/artifact"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/memory/pgmemory"
	"yanshi/internal/node"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/notify"
	"yanshi/internal/notify/pgnotify"
	"yanshi/internal/pg"
	"yanshi/internal/presence"
	"yanshi/internal/presence/pgpresence"
	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/session"
	"yanshi/internal/session/pgsnapshot"
	"yanshi/internal/usage"
	"yanshi/internal/usage/pgusage"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
	"yanshi/internal/workqueue/pgqueue"
	"yanshi/sdk/nodesdk"
)

// Stores 是一套存储实现。同一套中的各存储必须落在同一个存储系统上：Waking 是 Log 与 Queue 的原子组合。
type Stores struct {
	Log   eventlog.Log
	Queue workqueue.Queue
	// Waking 是 Log 与 Queue 的原子组合（session.Store.CommitAndWake）。
	Waking eventlog.WakingLog
	Dir    node.Directory
	Inbox  node.Inbox
	// 沙箱控制器的队列与共享状态
	SandboxQueue workqueue.Queue
	Ledger       nodesdk.Ledger
	Activity     sandbox.Activity
	ArtifactMeta artifact.MetaStore
	// Session 生命周期：索引、删除记录与清理队列
	Index        lifecycle.Index
	Deletions    lifecycle.Deletions
	JanitorQueue workqueue.Queue
	Memories     memory.Store
	Grants       memory.Grants
	// 投影快照
	Snapshots session.Snapshots
	Usage     usage.Store
	Presence  presence.Store
	Push      notify.Registry
}

// Memory 返回进程内存储：单进程开发、模拟测试与评测。
func Memory(clk clock.Clock) Stores {
	l, q := memlog.New(), memqueue.New(clk)
	return Stores{
		Log: l, Queue: q, Waking: l.Waking(q), Dir: node.NewMemDirectory(clk), Inbox: node.NewMemInbox(),
		SandboxQueue: memqueue.New(clk), Ledger: nodesdk.NewMemLedger(), Activity: sandbox.NewMemActivity(),
		ArtifactMeta: artifact.NewMemMeta(), Index: lifecycle.NewMemIndex(), Deletions: lifecycle.NewMemDeletions(),
		JanitorQueue: memqueue.New(clk), Memories: memory.NewMemStore(), Grants: memory.NewMemGrants(),
		Snapshots: session.NewMemSnapshots(), Usage: usage.NewMem(), Presence: presence.NewMem(), Push: notify.NewMemRegistry(),
	}
}

// Postgres 返回 PostgreSQL 上的存储；n 是该库的通知监听（须在运行）。多个进程共享同一套即构成分布式部署。
func Postgres(pool *pgxpool.Pool, n *pg.Notifier, clk clock.Clock) Stores {
	l, q := pglog.New(pool, n), pgqueue.New(pool, clk, pgqueue.Sessions).WithNotifier(n)
	return Stores{
		Log: l, Queue: q, Waking: l.Waking(q), Dir: pgnode.NewDirectory(pool, clk), Inbox: pgnode.NewInbox(pool, n),
		SandboxQueue: pgqueue.New(pool, clk, pgqueue.Sandboxes).WithNotifier(n), Ledger: pgsandbox.Ledger{Pool: pool},
		Activity: pgsandbox.Activity{Pool: pool}, ArtifactMeta: pgartifact.Meta{Pool: pool},
		Index: pglifecycle.Index{Pool: pool}, Deletions: pglifecycle.Deletions{Pool: pool, Notifier: n},
		JanitorQueue: pgqueue.New(pool, clk, pgqueue.Janitor), Memories: pgmemory.Store{Pool: pool}, Grants: pgmemory.Grants{Pool: pool},
		Snapshots: pgsnapshot.Store{Pool: pool}, Usage: pgusage.Store{Pool: pool}, Presence: pgpresence.Store{Pool: pool, Notifier: n},
		Push: pgnotify.Registry{Pool: pool},
	}
}
