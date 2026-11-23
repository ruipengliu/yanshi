package delivery_test

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xinli/delivery"
	"xinli/kernel"
	"xinli/recovery"
)

// tableKernel 的槽位表可在运行中改变；不在表中的分片读取得到 404 outbox_unknown_shard，与内核一致。
type tableKernel struct {
	*movedKernel
	tmu      sync.Mutex
	identity string
	table    map[int][]int
	reads    [3]atomic.Int32
	failed   [3]atomic.Int32
	deleted  map[string]bool
}

// StableSessions 与 SessionView 不再列出、读取已删除的 Session（逐 ID 读取 404）。
func (k *tableKernel) StableSessions(ctx context.Context, line, user, after string, limit int) ([]kernel.SessionInfo, error) {
	page, err := k.coldShardKernel.StableSessions(ctx, line, user, after, limit)
	k.tmu.Lock()
	defer k.tmu.Unlock()
	out := []kernel.SessionInfo{}
	for _, s := range page {
		if !k.deleted[s.ID] {
			out = append(out, s)
		}
	}
	return out, err
}

func (k *tableKernel) SessionView(ctx context.Context, line, id string) (kernel.SessionView, error) {
	k.tmu.Lock()
	gone := k.deleted[id]
	k.tmu.Unlock()
	if gone {
		return kernel.SessionView{}, kernel.ErrNotFound
	}
	return k.coldShardKernel.SessionView(ctx, line, id)
}

// fail 统计分片循环报告的失败；被移除的分片若仍在运行，恢复会反复报告缺少分片。
func (k *tableKernel) fail(f delivery.Failure) {
	if f.Shard >= 0 && f.Shard < len(k.failed) {
		k.failed[f.Shard].Add(1)
	}
}

func (k *tableKernel) set(identity string, table map[int][]int) {
	k.tmu.Lock()
	defer k.tmu.Unlock()
	k.identity, k.table = identity, table
}

func (k *tableKernel) OutboxHeads(context.Context, string) ([]kernel.OutboxShard, error) {
	k.tmu.Lock()
	defer k.tmu.Unlock()
	var out []kernel.OutboxShard
	for id, slots := range k.table {
		out = append(out, kernel.OutboxShard{Shard: id, Slots: slots, Status: "ready", Next: "H", Identity: k.identity})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Shard < out[j].Shard })
	return out, nil
}

func (k *tableKernel) Outbox(ctx context.Context, line string, id int, after string, limit int) (kernel.OutboxPage, error) {
	k.reads[id].Add(1)
	k.tmu.Lock()
	_, ok := k.table[id]
	k.tmu.Unlock()
	if !ok {
		return kernel.OutboxPage{}, &kernel.HTTPError{StatusCode: 404, Reason: "outbox_unknown_shard"}
	}
	return k.coldShardKernel.Outbox(ctx, line, min(id, 1), after, limit)
}

// conflictOnce 让分片 shard 的第一次退役提交冲突，模拟停掉的循环恰好还有一次提交在途。
type conflictOnce struct {
	recovery.Consumer
	shard int
	hit   atomic.Bool
}

func (c *conflictOnce) Commit(ctx context.Context, expected uint64, p recovery.Progress, batch recovery.Batch) error {
	if recovery.Shard(ctx) == c.shard && p.Phase == recovery.Heads && len(p.Positions) == 0 && c.hit.CompareAndSwap(false, true) {
		return recovery.ErrConflict
	}
	return c.Consumer.Commit(ctx, expected, p, batch)
}

func newTableKernel(f *fixture, identity string, table map[int][]int) *tableKernel {
	moved := &movedKernel{coldShardKernel: &coldShardKernel{recoveringKernel: &recoveringKernel{fakeKernel: f.kernel}}, last: map[string]uint64{}}
	return &tableKernel{movedKernel: moved, identity: identity, table: table, deleted: map[string]bool{}}
}

// waitProgress 等某个进度键满足条件；shard 为 -1 时是单库的进度。
func waitProgress(t *testing.T, f *fixture, ctx context.Context, shard int, what string, ok func(recovery.Progress) bool) recovery.Progress {
	t.Helper()
	if shard >= 0 {
		ctx = recovery.WithShard(ctx, shard)
	}
	deadline := time.Now().Add(5 * time.Second)
	var p recovery.Progress
	for time.Now().Before(deadline) {
		var err error
		if p, err = f.reopen().Load(ctx, "bl"); err != nil {
			t.Fatal(err)
		}
		if ok(p) {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("progress %d never %s: %+v", shard, what, p)
	return p
}

func following(identity string) func(recovery.Progress) bool {
	return func(p recovery.Progress) bool { return p.Phase == recovery.Follow && p.Identity == identity }
}

func retired(p recovery.Progress) bool { return p.Phase == recovery.Heads && len(p.Positions) == 0 }

// quiet 确认分片的循环已停：一段时间内既不读取，也不报告失败，而不是对 404 或缺少分片退避重试。
func quiet(t *testing.T, k *tableKernel, shard int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	reads, failed := k.reads[shard].Load(), k.failed[shard].Load()
	time.Sleep(1500 * time.Millisecond)
	if r, f := k.reads[shard].Load(), k.failed[shard].Load(); r != reads || f != failed {
		t.Fatalf("removed shard %d was still running: reads %d -> %d, failures %d -> %d", shard, reads, r, failed, f)
	}
}

func TestRemovedShardStopsAndRestartsWhenReadded(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		moved := coldSessions[1] // 槽位 512
		k := newTableKernel(f, "three", map[int][]int{0: {0}, 1: {}, 2: {512}})
		progress := &conflictOnce{Consumer: f.store, shard: 2}
		consumer := &delivery.Consumer{Progress: progress, Store: f.store, Kernel: k, Accounts: f.store}
		ctx, cancel := context.WithCancel(f.ctx)
		stop := delivery.Start(ctx, []string{"bl"}, consumer, f.worker, k.fail)
		defer stop()
		defer cancel()
		for id := range 3 {
			waitProgress(t, f, ctx, id, "followed", following("three"))
		}
		// 分片 2 合并进分片 1：槽位 512 迁走，分片 2 移出槽位表。
		k.mu.Lock()
		k.last[moved] = 2
		k.mu.Unlock()
		k.set("two", map[int][]int{0: {0}, 1: {512}})
		gone := waitProgress(t, f, ctx, 2, "retired", retired)
		if !progress.hit.Load() {
			t.Fatal("the first retirement did not conflict")
		}
		next := waitProgress(t, f, ctx, 1, "followed the new table", following("two"))
		if len(next.Slots) != 1 || next.Slots[0] != 512 {
			t.Fatalf("receiving shard kept the old slots: %+v", next)
		}
		if cp, err := f.reopen().Session(ctx, "bl", moved); err != nil || cp.Seq != 2 {
			t.Fatalf("moved session was not replayed by the receiving shard: %+v %v", cp, err)
		}
		quiet(t, k, 2)
		if p := waitProgress(t, f, ctx, 2, "retired", retired); p.Version != gone.Version {
			t.Fatalf("removed shard progress kept changing: %+v -> %+v", gone, p)
		}
		// 同一编号再次加入时从新的 H 开始，不沿用旧游标。
		k.set("three-again", map[int][]int{0: {0}, 1: {}, 2: {512}})
		again := waitProgress(t, f, ctx, 2, "followed after re-adding", following("three-again"))
		if again.Round <= gone.Round {
			t.Fatalf("re-added shard did not start a new round: %+v after %+v", again, gone)
		}
	})
}

func TestMergeToSingleShardRetiresShardProgress(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		moved := coldSessions[1]
		k := newTableKernel(f, "two", map[int][]int{0: {0}, 1: {512}})
		consumer := &delivery.Consumer{Progress: f.store, Store: f.store, Kernel: k, Accounts: f.store}
		ctx, cancel := context.WithCancel(f.ctx)
		stop := delivery.Start(ctx, []string{"bl"}, consumer, f.worker, k.fail)
		defer stop()
		defer cancel()
		waitProgress(t, f, ctx, 0, "followed", following("two"))
		waitProgress(t, f, ctx, 1, "followed", following("two"))
		k.mu.Lock()
		k.last[moved] = 2
		k.mu.Unlock()
		// 只剩分片 0 时按单库消费：两个分片的进度都退役，由单库进度重新对账。
		k.set("one", map[int][]int{0: {0, 512}})
		waitProgress(t, f, ctx, -1, "followed", following("one"))
		waitProgress(t, f, ctx, 0, "retired", retired)
		waitProgress(t, f, ctx, 1, "retired", retired)
		if cp, err := f.reopen().Session(ctx, "bl", moved); err != nil || cp.Seq != 2 {
			t.Fatalf("moved session was not replayed after the merge: %+v %v", cp, err)
		}
		quiet(t, k, 1)
	})
}

// 分片的轮次与单库的轮次各自计数：分片 1 第 1 轮访问过的 Session，不能因为单库也在第 1 轮就被确认阶段跳过。
// 它在合并期间被删除、删除信封随源出站行删除（未追平时 ADR 允许），只能靠单库一轮的确认阶段经 404 记为 Gone。
func TestMergeToSingleShardConfirmsSessionsSeenByShardRounds(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		moved := coldSessions[1]
		k := newTableKernel(f, "two", map[int][]int{0: {0}, 1: {512}})
		consumer := &delivery.Consumer{Progress: f.store, Store: f.store, Kernel: k, Accounts: f.store}
		ctx, cancel := context.WithCancel(f.ctx)
		stop := delivery.Start(ctx, []string{"bl"}, consumer, f.worker, k.fail)
		defer stop()
		defer cancel()
		waitProgress(t, f, ctx, 0, "followed", following("two"))
		shard := waitProgress(t, f, ctx, 1, "followed", following("two"))
		if cp, err := f.reopen().Session(ctx, "bl", moved); err != nil || cp.Seq != 1 {
			t.Fatalf("shard 1 did not replay the session: %+v %v", cp, err)
		}
		k.tmu.Lock()
		k.deleted[moved] = true
		k.tmu.Unlock()
		k.set("one", map[int][]int{0: {0, 512}})
		single := waitProgress(t, f, ctx, -1, "followed", following("one"))
		if single.Round != shard.Round {
			t.Fatalf("test needs colliding rounds: single %d, shard %d", single.Round, shard.Round)
		}
		if cp, err := f.reopen().Session(ctx, "bl", moved); err != nil || !cp.Gone {
			t.Fatalf("deleted session was not confirmed after switching to the single store: %+v %v", cp, err)
		}
	})
}
