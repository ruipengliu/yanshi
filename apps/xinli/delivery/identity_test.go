package delivery_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"xinli/delivery"
	"xinli/kernel"
	"xinli/recovery"
)

func TestReconcileIdentityResetsOnlyOnChange(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		ctx := recovery.WithShard(f.ctx, 1)
		consumer := &delivery.Consumer{Progress: f.store, Store: f.store, Kernel: &recoveringKernel{fakeKernel: f.kernel}, Accounts: f.store}
		// 尚无进度：不写入任何东西。
		if err := consumer.ReconcileIdentity(ctx, "bl", "a"); err != nil {
			t.Fatal(err)
		}
		if p, err := f.store.Load(ctx, "bl"); err != nil || p.Version != 0 {
			t.Fatalf("progress written without a previous round: %+v %v", p, err)
		}
		follow := recovery.Progress{BusinessLine: "bl", Version: 1, Revision: 3, Round: 2, Phase: recovery.Follow, Identity: "a",
			Positions: []recovery.Position{{Shard: 1, H: "H", After: "N"}}}
		if err := f.store.Commit(ctx, 0, follow, recovery.Batch{}); err != nil {
			t.Fatal(err)
		}
		if err := consumer.ReconcileIdentity(ctx, "bl", "a"); err != nil {
			t.Fatal(err)
		}
		if p, _ := f.store.Load(ctx, "bl"); p.Version != 1 || p.Phase != recovery.Follow || p.Positions[0].After != "N" {
			t.Fatalf("unchanged identity reset progress: %+v", p)
		}
		if err := consumer.ReconcileIdentity(ctx, "bl", "b"); err != nil {
			t.Fatal(err)
		}
		p, err := f.reopen().Load(ctx, "bl")
		if err != nil || p.Phase != recovery.Heads || p.Version != 2 || p.Revision != 4 || p.Round != 2 || len(p.Positions) != 0 {
			t.Fatalf("changed identity did not restart reconciliation: %+v %v", p, err)
		}
		// 已在 Heads：下一轮取 H 时会记下新摘要，不再重复写入。
		if err := consumer.ReconcileIdentity(ctx, "bl", "c"); err != nil {
			t.Fatal(err)
		}
		if p, _ := f.store.Load(ctx, "bl"); p.Version != 2 {
			t.Fatalf("progress in heads was rewritten: %+v", p)
		}
	})
}

// movedKernel 模拟迁槽后漏了换代：游标照常有效、出站流没有送来迁移前的事件，只有 /v1/outbox/shards 的槽位与摘要变了。
type movedKernel struct {
	*coldShardKernel
	mu       sync.Mutex
	identity string
	slots    [2][]int
	last     map[string]uint64
}

func (k *movedKernel) OutboxHeads(context.Context, string) ([]kernel.OutboxShard, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return []kernel.OutboxShard{
		{Shard: 0, Slots: k.slots[0], Status: "ready", Next: "H0", Identity: k.identity},
		{Shard: 1, Slots: k.slots[1], Status: "ready", Next: "H1", Identity: k.identity},
	}, nil
}

func (k *movedKernel) Events(ctx context.Context, line, id string, after uint64, limit int) (kernel.EventPage, error) {
	page, err := k.coldShardKernel.Events(ctx, line, id, after, limit)
	k.mu.Lock()
	last := max(k.last[id], 1)
	k.mu.Unlock()
	// 第一个事件之后只追加不带信封的事件：补读推进 Seq，不产生发送。
	for seq := max(after+1, 2); seq <= last; seq++ {
		page.Events = append(page.Events, json.RawMessage(fmt.Sprintf(`{"session_id":%q,"seq":"%d"}`, id, seq)))
	}
	page.Last = last
	return page, err
}

func TestShardTableChangeWithoutRotationReconciles(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		moved := coldSessions[1] // 槽位 512
		k := &movedKernel{coldShardKernel: &coldShardKernel{recoveringKernel: &recoveringKernel{fakeKernel: f.kernel}},
			identity: "before", slots: [2][]int{{0, 512}, {}}, last: map[string]uint64{}}
		consumer := &delivery.Consumer{Progress: f.store, Store: f.store, Kernel: k, Accounts: f.store}
		ctx, cancel := context.WithCancel(f.ctx)
		stop := delivery.Start(ctx, []string{"bl"}, consumer, f.worker, nil)
		defer stop()
		defer cancel()
		wait := func(shard int, identity string, round uint64) recovery.Progress {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			var p recovery.Progress
			for time.Now().Before(deadline) {
				var err error
				p, err = f.reopen().Load(recovery.WithShard(ctx, shard), "bl")
				if err != nil {
					t.Fatal(err)
				}
				if p.Phase == recovery.Follow && p.Identity == identity && p.Round >= round {
					return p
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("shard %d did not reach follow with identity %q: %+v", shard, identity, p)
			return p
		}
		wait(0, "before", 1)
		first := wait(1, "before", 1)
		if cp, err := f.reopen().Session(ctx, "bl", moved); err != nil || cp.Seq != 1 {
			t.Fatalf("shard 0 did not replay the session before the move: %+v %v", cp, err)
		}
		// 迁移前写入的事件没有经出站流送达，随后槽位 512 改到分片 1，但分片没有换代。
		k.mu.Lock()
		k.last[moved] = 2
		k.identity, k.slots = "after", [2][]int{{0}, {512}}
		k.mu.Unlock()
		second := wait(1, "after", first.Round+1)
		if len(second.Slots) != 1 || second.Slots[0] != 512 {
			t.Fatalf("new round kept the old slots: %+v", second)
		}
		if cp, err := f.reopen().Session(ctx, "bl", moved); err != nil || cp.Seq != 2 {
			t.Fatalf("moved session was not replayed after the table change: %+v %v", cp, err)
		}
	})
}
