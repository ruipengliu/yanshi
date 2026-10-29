package delivery_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"xinli/delivery"
	"xinli/kernel"
	"xinli/recovery"
)

type shardKernel struct {
	*recoveringKernel
	offline   atomic.Bool
	healthy   atomic.Int32
	recovered atomic.Int32
}

type coldShardKernel struct {
	*recoveringKernel
	polls [2]atomic.Int32
}

var coldSessions = []string{"ses_v1_000_000_first", "ses_v1_0g0_000_second"}

func (k *coldShardKernel) OutboxHeads(context.Context, string) ([]kernel.OutboxShard, error) {
	return []kernel.OutboxShard{
		{Shard: 0, Slots: []int{0}, Status: "ready", Next: "H0"},
		{Shard: 1, Slots: []int{512}, Status: "ready", Next: "H1"},
	}, nil
}

func (k *coldShardKernel) Outbox(ctx context.Context, _ string, id int, _ string, _ int) (kernel.OutboxPage, error) {
	select {
	case <-ctx.Done():
		return kernel.OutboxPage{}, ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	return kernel.OutboxPage{Events: []kernel.Envelope{}, Next: fmt.Sprintf("%d-%d", id, k.polls[id].Add(1))}, nil
}

func (k *coldShardKernel) StableSessions(_ context.Context, _, user, after string, limit int) ([]kernel.SessionInfo, error) {
	page := []kernel.SessionInfo{}
	for _, id := range coldSessions {
		if id > after && len(page) < limit {
			page = append(page, kernel.SessionInfo{ID: id, EndUser: user})
		}
	}
	return page, nil
}

func (k *coldShardKernel) SessionView(_ context.Context, line, id string) (kernel.SessionView, error) {
	return kernel.SessionView{ID: id, BusinessLine: line, EndUser: k.owner.EndUser}, nil
}

func (k *coldShardKernel) Events(_ context.Context, line, id string, after uint64, _ int) (kernel.EventPage, error) {
	page := kernel.EventPage{Events: []json.RawMessage{}, Last: 1}
	if after == 0 {
		page.Events = []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"session_id":%q,"seq":"1","session_created":{"business_line":%q,"end_user":%q,"agent":{"name":"xinli-assistant","version":"1"}}}`, id, line, k.owner.EndUser))}
	}
	return page, nil
}

func TestShardColdStartReachesDurableFollow(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		k := &coldShardKernel{recoveringKernel: &recoveringKernel{fakeKernel: f.kernel}}
		consumer := &delivery.Consumer{Progress: f.store, Store: f.store, Kernel: k, Accounts: f.store}
		ctx, cancel := context.WithCancel(f.ctx)
		stop := delivery.Start(ctx, []string{"bl"}, consumer, f.worker, nil)
		defer stop()
		defer cancel()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ready := true
			for id := range 2 {
				p, err := f.reopen().Load(recovery.WithShard(ctx, id), "bl")
				if err != nil {
					t.Fatal(err)
				}
				ready = ready && p.Phase == recovery.Follow && p.Revision > 0 && len(p.Positions) == 1 && p.Positions[0].Shard == id
			}
			if ready {
				for id, sid := range coldSessions {
					cp, err := f.reopen().Session(ctx, "bl", sid)
					if err != nil || cp.Seq != 1 || k.polls[id].Load() == 0 {
						t.Fatalf("shard %d did not recover and poll: %+v %v", id, cp, err)
					}
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		for id := range 2 {
			p, err := f.store.Load(recovery.WithShard(ctx, id), "bl")
			t.Logf("shard=%d phase=%s version=%d revision=%d err=%v", id, p.Phase, p.Version, p.Revision, err)
		}
		t.Fatal("fresh shard consumers did not reach durable follow")
	})
}

func (k *shardKernel) OutboxHeads(context.Context, string) ([]kernel.OutboxShard, error) {
	return []kernel.OutboxShard{{Shard: 0, Status: "unavailable"}, {Shard: 1, Status: "ready", Next: "H1"}}, nil
}
func (k *shardKernel) Outbox(ctx context.Context, _ string, id int, _ string, _ int) (kernel.OutboxPage, error) {
	if id == 0 && k.offline.Load() {
		<-ctx.Done()
		return kernel.OutboxPage{}, ctx.Err()
	}
	n := k.healthy.Add(1)
	if id == 0 {
		n = k.recovered.Add(1)
	}
	return kernel.OutboxPage{Events: []kernel.Envelope{}, Next: fmt.Sprintf("%d-%d", id, n)}, nil
}

func TestShardPollDoesNotWaitForUnavailableShard(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		for id := range 2 {
			ctx := recovery.WithShard(f.ctx, id)
			p := recovery.Progress{BusinessLine: "bl", Version: 1, Phase: recovery.Follow, Positions: []recovery.Position{{Shard: id, H: "H", After: "H"}}}
			if err := f.store.Commit(ctx, 0, p, recovery.Batch{}); err != nil {
				t.Fatal(err)
			}
		}
		k := &shardKernel{recoveringKernel: &recoveringKernel{fakeKernel: f.kernel}}
		k.offline.Store(true)
		consumer := &delivery.Consumer{Progress: f.store, Store: f.store, Kernel: k, Accounts: f.store}
		ctx, cancel := context.WithCancel(f.ctx)
		stop := delivery.Start(ctx, []string{"bl"}, consumer, f.worker, nil)
		defer stop()
		defer cancel()
		deadline := time.Now().Add(2 * time.Second)
		for k.healthy.Load() < 3 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if k.healthy.Load() < 3 {
			t.Fatal("healthy shard waited for the blocked long poll")
		}
		p, err := f.reopen().Load(recovery.WithShard(ctx, 1), "bl")
		if err != nil || p.Positions[0].After == "H" {
			t.Fatalf("healthy cursor not durable: %+v %v", p, err)
		}
		p, err = f.store.Load(recovery.WithShard(ctx, 0), "bl")
		if err != nil || p.Positions[0].After != "H" {
			t.Fatal("unavailable cursor advanced")
		}
	})
}
