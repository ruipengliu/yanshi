package app_test

import (
	"context"
	"testing"

	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/artifact/artifacttest"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/eventlogtest"
	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/lifecycletest"
	"yanshi/internal/memory"
	"yanshi/internal/memory/memorytest"
	"yanshi/internal/node"
	"yanshi/internal/node/nodetest"
	"yanshi/internal/notify"
	"yanshi/internal/notify/notifytest"
	"yanshi/internal/outbox/outboxtest"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/presence"
	"yanshi/internal/presence/presencetest"
	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/sandboxtest"
	"yanshi/internal/session"
	"yanshi/internal/session/snapshottest"
	"yanshi/internal/shard"
	"yanshi/internal/usage"
	"yanshi/internal/usage/tiertest"
	"yanshi/internal/usage/usagetest"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/workqueuetest"
)

// 旧一致性夹具使用 s1/u 等 legacy 键，保留全部槽到 0；两个实际库都参与离线操作。
// 新格式跨片隔离另由 TestShardedRouting 覆盖，不能把 legacy 夹具重命名后改变既有契约。
func suiteLocator(t *testing.T) shard.Locator {
	t.Helper()
	l, err := shard.ParseDeployment([]byte(`{"table":{"shards":[0,1],"ranges":[{"first":0,"last":1023,"shard":0}]},"compatibility":{"version":"suite","legacy_data_in_shard0":true,"entries":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestShardedConsistency(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var dsns []string
			if backend == "postgres" {
				dsns = []string{pgtest.Database(t), pgtest.Database(t)}
			}
			locator := suiteLocator(t)
			fresh := func(t *testing.T, c clock.Clock) app.Stores {
				t.Helper()
				parts := map[int]app.Stores{}
				for id := range 2 {
					if backend == "memory" {
						parts[id] = app.MemoryShard(c, locator)
					} else {
						p, n := pgtest.FreshAt(t, dsns[id])
						parts[id] = app.PostgresShard(p, n, c, locator, id)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				stores, err := app.Sharded(ctx, locator, parts, app.ShardOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return stores
			}
			t.Run("Log", func(t *testing.T) {
				eventlogtest.Run(t, func(t *testing.T) eventlog.Log { return fresh(t, clock.Real{}).Log })
			})
			t.Run("Queue", func(t *testing.T) {
				workqueuetest.Run(t, func(t *testing.T, c clock.Clock) workqueue.Queue { return fresh(t, c).Queue })
			})
			t.Run("Dir", func(t *testing.T) {
				nodetest.RunDirectory(t, func(t *testing.T, c clock.Clock) node.Directory { return fresh(t, c).Dir })
			})
			t.Run("Inbox", func(t *testing.T) {
				nodetest.RunInbox(t, func(t *testing.T) node.Inbox { return fresh(t, clock.Real{}).Inbox })
			})
			t.Run("Ledger", func(t *testing.T) {
				sandboxtest.RunLedger(t, func(t *testing.T) sandbox.Ledger { return fresh(t, clock.Real{}).Ledger })
			})
			t.Run("Activity", func(t *testing.T) {
				sandboxtest.RunActivity(t, func(t *testing.T) sandbox.Activity { return fresh(t, clock.Real{}).Activity })
			})
			t.Run("ArtifactMeta", func(t *testing.T) {
				artifacttest.RunMeta(t, func(t *testing.T) artifact.MetaStore { return fresh(t, clock.Real{}).ArtifactMeta })
			})
			t.Run("Index", func(t *testing.T) {
				lifecycletest.RunIndex(t, func(t *testing.T) lifecycle.Index { return fresh(t, clock.Real{}).Index })
			})
			t.Run("Deletions", func(t *testing.T) {
				lifecycletest.RunDeletions(t, func(t *testing.T) lifecycle.Deletions { return fresh(t, clock.Real{}).Deletions })
			})
			t.Run("Memories", func(t *testing.T) {
				memorytest.RunStore(t, func(t *testing.T) memory.Store { return fresh(t, clock.Real{}).Memories })
			})
			t.Run("Access", func(t *testing.T) {
				memorytest.RunAccess(t, func(t *testing.T) memory.Store { return fresh(t, clock.Real{}).Memories })
			})
			t.Run("Grants", func(t *testing.T) {
				memorytest.RunGrants(t, func(t *testing.T) memory.Grants { return fresh(t, clock.Real{}).Grants })
			})
			t.Run("Snapshots", func(t *testing.T) {
				snapshottest.Run(t, func(t *testing.T) session.Snapshots { return fresh(t, clock.Real{}).Snapshots })
			})
			t.Run("Usage", func(t *testing.T) {
				usagetest.Run(t, func(t *testing.T) usage.Store { return fresh(t, clock.Real{}).Usage })
			})
			t.Run("Tiers", func(t *testing.T) {
				tiertest.Run(t, func(t *testing.T) usage.TierStore { return fresh(t, clock.Real{}).Tiers })
			})
			t.Run("Presence", func(t *testing.T) {
				presencetest.Run(t, func(t *testing.T) presence.Store { return fresh(t, clock.Real{}).Presence })
			})
			t.Run("Push", func(t *testing.T) {
				notifytest.Run(t, func(t *testing.T) notify.Registry { return fresh(t, clock.Real{}).Push })
			})
			t.Run("Waking", func(t *testing.T) {
				eventlogtest.RunWaking(t, func(t *testing.T, c clock.Clock) eventlogtest.WakingStores {
					s := fresh(t, c)
					return eventlogtest.WakingStores{Log: s.Log, Queue: s.Queue, Waking: s.Waking}
				})
			})
			t.Run("StableDeletion", func(t *testing.T) {
				s := fresh(t, clock.Real{})
				lifecycletest.RunStableDeletion(t, s.Index, s.Deletions)
			})
			t.Run("OutboxAtomic", func(t *testing.T) {
				outboxtest.Run(t, func(t *testing.T) outboxtest.Stores {
					s := fresh(t, clock.Real{})
					return outboxtest.Stores{Log: s.Log, Waking: s.Waking, Queue: s.Queue, Deletions: s.Deletions, Outbox: s.Outbox}
				})
			})
		})
	}
}
