package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/node"
	"yanshi/internal/notify"
	"yanshi/internal/outbox"
	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/presence"
	"yanshi/internal/shard"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

func splitLocator(t *testing.T) shard.Locator {
	t.Helper()
	l, err := shard.ParseDeployment([]byte(`{"table":{"shards":[0,1],"ranges":[{"first":0,"last":511,"shard":0},{"first":512,"last":1023,"shard":1}]},"compatibility":{"version":"dual","legacy_data_in_shard0":false,"entries":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func keysIn(t *testing.T, l shard.Locator, target int, eu string) string {
	t.Helper()
	for i := range 10000 {
		sid, err := ids.SessionID(fmt.Sprintf("key-%d", i), eu)
		if err != nil {
			t.Fatal(err)
		}
		id, err := l.Locate(shard.Target{Kind: shard.SessionData, Key: sid})
		if err != nil {
			t.Fatal(err)
		}
		if id == target {
			return sid
		}
	}
	t.Fatal("no key")
	return ""
}

func userIn(l shard.Locator, target int) string {
	for i := 0; ; i++ {
		user := fmt.Sprintf("user-%d", i)
		id, _ := l.Locate(shard.Target{Kind: shard.EndUser, Key: user})
		if id == target {
			return user
		}
	}
}

func TestShardedRouting(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			l := splitLocator(t)
			parts := map[int]app.Stores{}
			for id := range 2 {
				parts[id] = app.MemoryShard(clk, l)
				if backend == "postgres" {
					p, n := pgtest.FreshDatabase(t)
					parts[id] = app.PostgresShard(p, n, clk, l, id)
				}
			}
			rctx, cancel := context.WithCancel(ctx)
			t.Cleanup(cancel)
			s, err := app.Sharded(rctx, l, parts, app.ShardOptions{})
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(err)
			eu := userIn(l, 1)
			sids := []string{keysIn(t, l, 0, eu), keysIn(t, l, 1, eu)}
			must(s.Deletions.CreateRequest(ctx, &lifecycle.Request{ID: "request", BusinessLine: "bl", CreatedAt: clk.Now()}))
			for id, sid := range sids {
				_, err := s.Waking.AppendAndEnqueue(ctx, sid, 0, workqueue.Class{}, nil, &v1.Event{})
				must(err)
				rows, err := parts[1-id].Log.Read(ctx, sid, 0, 10)
				must(err)
				if len(rows) != 0 {
					t.Fatal("log written to wrong shard")
				}
				_, err = s.Waking.AppendAndEnqueue(ctx, sid, 0, workqueue.Class{}, nil, &v1.Event{})
				if !errors.Is(err, eventlog.ErrConflict) {
					t.Fatalf("conflict: %v", err)
				}
				must(s.Index.Put(ctx, &lifecycle.Session{ID: sid, EndUser: eu, BusinessLine: "bl", CreatedAt: clk.Now(), LastInputAt: clk.Now()}))
				entry, err := s.Index.LookupSession(ctx, sid)
				must(err)
				if entry.EndUser != eu {
					t.Fatal("index ownership changed")
				}
				if _, err := parts[0].Index.Get(ctx, eu, sid); !errors.Is(err, lifecycle.ErrNotFound) {
					t.Fatal("index did not follow EndUser")
				}
				aid, err := ids.WithSession("art", fmt.Sprintf("art-%d", id), sid)
				must(err)
				must(s.ArtifactMeta.Create(ctx, &artifact.Meta{ID: aid, SessionID: sid, CreatedAt: clk.Now()}))
				meta, err := s.ArtifactMeta.Lookup(ctx, aid)
				must(err)
				if meta.SessionID != sid {
					t.Fatal("artifact did not follow Session")
				}
				if _, err := s.ArtifactMeta.Get(ctx, sids[1-id], aid); !errors.Is(err, artifact.ErrNotFound) {
					t.Fatalf("foreign artifact: %v", err)
				}
				_, err = s.Deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: sid, BusinessLine: "bl", RequestID: "request", RequestedAt: clk.Now()})
				must(err)
			}
			for expected := range 2 {
				lease, err := s.Queue.Claim(ctx, "worker", time.Minute, workqueue.Pool{})
				must(err)
				if !lease.Sharded || lease.Shard != expected {
					t.Fatalf("round robin: %+v", lease)
				}
				forged := *lease
				forged.Shard = 1 - lease.Shard
				if err := s.Queue.Release(ctx, &forged, true); !errors.Is(err, workqueue.ErrLeaseLost) {
					t.Fatalf("foreign lease accepted: %v", err)
				}
				must(s.Queue.Renew(ctx, lease, time.Minute))
				must(s.Queue.Release(ctx, lease, true))
			}
			must(s.Deletions.Complete(ctx, sids[1], clk.Now()))
			req, err := s.Deletions.Request(ctx, "request")
			must(err)
			if req.Sessions != 2 || req.Completed != 1 {
				t.Fatalf("progress: %+v", req)
			}
			if _, err := parts[1].Deletions.(lifecycle.RequestProgress).RequestInfo(ctx, "request"); !errors.Is(err, lifecycle.ErrNotFound) {
				t.Fatal("request is not fixed in shard 0")
			}
			for id := range 2 {
				user := userIn(l, id)
				_, _, err := s.Dir.Register(ctx, node.Info{NodeID: "same-node", Scope: node.Scope{BusinessLine: "bl", EndUser: user}, Label: "phone"})
				must(err)
				must(s.Tiers.Put(ctx, "bl", user, fmt.Sprint(id), clk.Now()))
				must(s.Usage.Record(ctx, &usage.Entry{ID: fmt.Sprintf("use-%d", id), BusinessLine: "bl", EndUser: user, Kind: usage.Model, Cost: int64(id + 1), At: clk.Now()}))
			}
			// 同名设备、连接和调用分属两个真实键，读回与删除必须互不影响。
			for id := range 2 {
				user := userIn(l, id)
				info, err := s.Dir.Get(ctx, user, "same-node")
				must(err)
				if info.Scope.EndUser != user {
					t.Fatal("directory crossed EndUser")
				}
				tier, err := s.Tiers.Get(ctx, "bl", user)
				must(err)
				if tier != fmt.Sprint(id) {
					t.Fatal("tier crossed EndUser")
				}
				must(s.Inbox.Put(ctx, user, "same-node", &v1.Invoke{SessionId: sids[id], CallId: "same-call"}))
				must(s.Push.Register(ctx, &notify.Device{BusinessLine: "bl", EndUser: user, DeviceID: "same-device", Token: fmt.Sprint(id)}, clk.Now()))
				mid, err := ids.WithEndUser("mem", "same-token", user)
				must(err)
				must(s.Memories.Put(ctx, &memory.Memory{ID: mid, BusinessLine: "bl", EndUser: user, Category: "preference", Content: fmt.Sprint(id), SourceSession: sids[id], CreatedAt: clk.Now(), UpdatedAt: clk.Now()}, nil))
				gid, err := ids.WithEndUser("grant", "same-token", user)
				must(err)
				must(s.Grants.Create(ctx, &memory.Grant{ID: gid, EndUser: user, From: "bl", To: "reader", Categories: []memory.Category{"preference"}, CreatedAt: clk.Now()}))
				must(s.Presence.Put(ctx, &presence.Entry{SessionID: sids[id], ConnID: "same-connection", Label: fmt.Sprint(id), Expires: clk.Now().Add(time.Hour)}))
			}
			must(s.Inbox.Remove(ctx, userIn(l, 0), "same-node", "same-call"))
			must(s.Push.DeleteEndUser(ctx, "bl", userIn(l, 0)))
			must(s.Memories.DeleteEndUser(ctx, "bl", userIn(l, 0)))
			must(s.Grants.DeleteBusinessLine(ctx, userIn(l, 0), "bl"))
			must(s.Presence.DeleteSession(ctx, sids[0]))
			pending, _, err := s.Inbox.Pending(ctx, userIn(l, 1), "same-node")
			must(err)
			if len(pending) != 1 || pending[0].GetSessionId() != sids[1] {
				t.Fatal("inbox crossed shard")
			}
			devices, err := s.Push.List(ctx, "bl", userIn(l, 1))
			must(err)
			if len(devices) != 1 || devices[0].Token != "1" {
				t.Fatal("push crossed shard")
			}
			mid, err := ids.WithEndUser("mem", "same-token", userIn(l, 1))
			must(err)
			mem, err := s.Memories.Get(ctx, userIn(l, 1), mid)
			must(err)
			if mem.Content != "1" {
				t.Fatal("memory crossed shard")
			}
			grants, err := s.Grants.ToReader(ctx, userIn(l, 1), "reader")
			must(err)
			if len(grants) != 1 {
				t.Fatal("grant crossed shard")
			}
			entries, _, err := s.Presence.List(ctx, sids[1], clk.Now())
			must(err)
			if len(entries) != 1 || entries[0].Label != "1" {
				t.Fatal("presence crossed shard")
			}
			spent, err := s.Usage.Spent(ctx, "bl", "", clk.Now(), clk.Now().Add(time.Hour))
			must(err)
			if spent != 3 {
				t.Fatalf("partial total: %d", spent)
			}
			page, next, err := s.Index.List(ctx, "bl", "", "", 1)
			must(err)
			if len(page) != 1 || next == "" {
				t.Fatal("global pagination first page")
			}
			page2, _, err := s.Index.List(ctx, "bl", "", next, 1)
			must(err)
			if len(page2) != 1 || page[0].ID == page2[0].ID {
				t.Fatal("global pagination lost or repeated row")
			}
			for id := range 2 {
				page, err := s.Outbox.Read(ctx, outbox.Request{BusinessLine: "bl", Shard: id})
				must(err)
				if page.Next == "" {
					t.Fatal("missing shard cursor")
				}
			}
		})
	}
}

// 卡住的分片遵守 context 取消；健康库认领和在线写入无需等待整轮维护。
type stalledQueue struct{ workqueue.Queue }

func (s stalledQueue) Claim(ctx context.Context, _ string, _ time.Duration, _ workqueue.Pool) (*workqueue.Lease, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s stalledQueue) Promote(ctx context.Context, t time.Time, n int) (int, error) {
	return s.Queue.(workqueue.Deferrer).Promote(ctx, t, n)
}
func (s stalledQueue) Horizon() time.Duration { return s.Queue.(workqueue.Deferrer).Horizon() }
func (s stalledQueue) Contains(ctx context.Context, sid string) (bool, error) {
	return s.Queue.(workqueue.Inspector).Contains(ctx, sid)
}
func (s stalledQueue) Depths(ctx context.Context) ([]workqueue.Depth, error) {
	return s.Queue.(workqueue.Stats).Depths(ctx)
}

type failedUsage struct{ usage.Store }

func (s failedUsage) Spent(context.Context, string, string, time.Time, time.Time) (int64, error) {
	return 0, errors.New("unavailable")
}

func TestShardFailureDoesNotBlockHealthyQueue(t *testing.T) {
	l := splitLocator(t)
	clk := clock.Real{}
	bad, good := app.MemoryShard(clk, l), app.MemoryShard(clk, l)
	bad.Queue = stalledQueue{bad.Queue}
	bad.Usage = failedUsage{bad.Usage}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := app.Sharded(ctx, l, map[int]app.Stores{0: bad, 1: good}, app.ShardOptions{ClaimTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	sid := keysIn(t, l, 1, userIn(l, 1))
	if err := s.Queue.Enqueue(ctx, sid, workqueue.Class{}); err != nil {
		t.Fatal(err)
	}
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	lease, err := s.Queue.Claim(bounded, "w", time.Minute, workqueue.Pool{})
	if err != nil || lease.Shard != 1 {
		t.Fatalf("healthy claim: %+v %v", lease, err)
	}
	if _, err := s.Log.Append(ctx, sid, 0, nil, &v1.Event{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Usage.Spent(ctx, "bl", "", time.Time{}, time.Time{}); err == nil {
		t.Fatal("partial total accepted")
	}
	if _, err := s.Usage.Spent(ctx, "bl", userIn(l, 1), time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestShardedTransactionRejectsOtherPool(t *testing.T) {
	p0, n0 := pgtest.FreshDatabase(t)
	p1, n1 := pgtest.FreshDatabase(t)
	l := splitLocator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := app.Sharded(ctx, l, map[int]app.Stores{0: app.PostgresShard(p0, n0, clock.Real{}, l, 0), 1: app.PostgresShard(p1, n1, clock.Real{}, l, 1)}, app.ShardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := p0.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tctx := pg.WithTx(ctx, p0, tx)
	if err := s.Tiers.Put(tctx, "bl", userIn(l, 1), "pro", time.Now()); err == nil {
		t.Fatal("cross-pool operation accepted")
	}
	if _, err := pg.Exec(tctx, p1, "SELECT 1"); err == nil {
		t.Fatal("cross-pool Exec accepted")
	}
	if err := s.Tiers.Put(tctx, "bl", userIn(l, 0), "pro", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if tier, err := s.Tiers.Get(ctx, "bl", userIn(l, 0)); err != nil || tier != "" {
		t.Fatalf("same-pool write escaped transaction: %q %v", tier, err)
	}

}

func TestShardedEndUserCleanupSharesTransaction(t *testing.T) {
	l := splitLocator(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	parts := map[int]app.Stores{}
	for id := range 2 {
		pool, notifier := pgtest.FreshDatabase(t)
		cfg := pool.Config()
		cfg.MaxConns = 1
		limited, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(limited.Close)
		parts[id] = app.PostgresShard(limited, notifier, clk, l, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := app.Sharded(ctx, l, parts, app.ShardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for id := range 2 {
		eu := userIn(l, id)
		if err := s.Tiers.Put(ctx, "bl", eu, "pro", clk.Now()); err != nil {
			t.Fatal(err)
		}
		b, err := s.Deletions.MarkEndUser(ctx, &lifecycle.EndUserTombstone{BusinessLine: "bl", EndUser: eu, RequestID: fmt.Sprintf("request-%d", id), RequestedAt: clk.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Deletions.SealEndUser(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := s.Deletions.CompleteEndUser(ctx, b, clk.Now(), clk.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	failed := errors.New("holder failed after tier deletion")
	fail := true
	cleanup := func(ctx context.Context, bl, eu string) error {
		// 每库只有一条连接，路由后的清理必须复用屏障事务；失败时档位与屏障一起回滚。
		if err := s.Tiers.Delete(ctx, bl, eu); err != nil {
			return err
		}
		if fail && eu == userIn(l, 0) {
			return failed
		}
		return nil
	}
	if err := s.Deletions.PruneEndUsers(ctx, clk.Now().Add(time.Hour), 10, cleanup); !errors.Is(err, failed) {
		t.Fatalf("cleanup failure = %v", err)
	}
	for id := range 2 {
		eu := userIn(l, id)
		want := ""
		if id == 0 {
			want = "pro"
		}
		if got, err := s.Tiers.Get(ctx, "bl", eu); err != nil || got != want {
			t.Fatalf("shard %d tier = %q, want %q: %v", id, got, want, err)
		}
		_, err := s.Deletions.GetEndUser(ctx, "bl", eu)
		if id == 0 && err != nil || id == 1 && !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("shard %d barrier: %v", id, err)
		}
	}
	fail = false
	if err := s.Deletions.PruneEndUsers(ctx, clk.Now().Add(time.Hour), 10, cleanup); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Tiers.Get(ctx, "bl", userIn(l, 0)); err != nil || got != "" {
		t.Fatalf("retry retained tier: %q, %v", got, err)
	}
	if _, err := s.Deletions.GetEndUser(ctx, "bl", userIn(l, 0)); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("retry retained barrier: %v", err)
	}
}

func TestAllQueuesReturnLeaseToOrigin(t *testing.T) {
	for _, name := range []string{"sessions", "sandboxes", "janitor"} {
		t.Run(name, func(t *testing.T) {
			l := splitLocator(t)
			clk := clock.NewFake(time.Now())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			parts := map[int]app.Stores{0: app.MemoryShard(clk, l), 1: app.MemoryShard(clk, l)}
			s, err := app.Sharded(ctx, l, parts, app.ShardOptions{})
			if err != nil {
				t.Fatal(err)
			}
			q := s.Queue
			if name == "sandboxes" {
				q = s.SandboxQueue
			}
			if name == "janitor" {
				q = s.JanitorQueue
			}
			for id := range 2 {
				key := keysIn(t, l, id, "owner")
				if name == "sandboxes" {
					key = "sbx_" + key
				}
				if err := q.Enqueue(ctx, key, workqueue.Class{}); err != nil {
					t.Fatal(err)
				}
			}
			for id := range 2 {
				lease, err := q.Claim(ctx, "w", time.Minute, workqueue.Pool{})
				if err != nil || lease.Shard != id {
					t.Fatalf("claim %d: %+v %v", id, lease, err)
				}
				if err := q.Park(ctx, lease, clk.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			clk.Advance(time.Second)
			for id := range 2 {
				lease, err := q.Claim(ctx, "w2", time.Minute, workqueue.Pool{})
				if err != nil || lease.Shard != id {
					t.Fatalf("reclaim %d: %+v %v", id, lease, err)
				}
				if err := q.Renew(ctx, lease, time.Minute); err != nil {
					t.Fatal(err)
				}
				if err := q.Release(ctx, lease, true); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPostgresShardUnavailable(t *testing.T) {
	p0, n0 := pgtest.FreshDatabase(t)
	p1, n1 := pgtest.FreshDatabase(t)
	l := splitLocator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sql0, err := pgxpool.NewWithConfig(ctx, p0.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sql0.Close)
	s, err := app.Sharded(ctx, l, map[int]app.Stores{0: app.PostgresShard(sql0, n0, clock.Real{}, l, 0), 1: app.PostgresShard(p1, n1, clock.Real{}, l, 1)}, app.ShardOptions{ClaimTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// 独立 SQL 池关闭不影响测试 Notifier 持有的直连池；这是真实的目标库不可访问。
	sql0.Close()
	bad := keysIn(t, l, 0, "owner")
	if _, err := s.Log.Append(ctx, bad, 0, nil, &v1.Event{}); err == nil {
		t.Fatal("unavailable shard request succeeded")
	}
	if _, err := s.Usage.Spent(ctx, "bl", "", time.Time{}, time.Time{}); !errors.Is(err, shard.ErrUnavailable) {
		t.Fatalf("unavailable shard omitted or misclassified: %v", err)
	}
	good := keysIn(t, l, 1, "owner")
	if _, err := s.Waking.AppendAndEnqueue(ctx, good, 0, workqueue.Class{}, nil, &v1.Event{}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Queue.Claim(ctx, "w", time.Minute, workqueue.Pool{})
	if err != nil || lease.SessionID != good {
		t.Fatalf("healthy shard: %+v %v", lease, err)
	}
}

func TestShardedQueueMergesSignals(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			l := splitLocator(t)
			parts := map[int]app.Stores{}
			for id := range 2 {
				parts[id] = app.MemoryShard(clock.Real{}, l)
				if backend == "postgres" {
					p, n := pgtest.FreshDatabase(t)
					parts[id] = app.PostgresShard(p, n, clock.Real{}, l, id)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			s, err := app.Sharded(ctx, l, parts, app.ShardOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ready := s.Queue.(workqueue.Signaler).Ready()
			for id := range 2 {
				// 先排空订阅建立时的重查提示，再从实际分片直接入队。
				timer := time.NewTimer(30 * time.Millisecond)
			drain:
				for {
					select {
					case <-ready:
					case <-timer.C:
						break drain
					}
				}
				sid := keysIn(t, l, id, "owner")
				if err := parts[id].Queue.Enqueue(ctx, sid, workqueue.Class{}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-ready:
				case <-time.After(10 * time.Second):
					t.Fatalf("shard %d notification not forwarded", id)
				}
			}
		})
	}
}
