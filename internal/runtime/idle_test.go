package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/model/echo"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// countingQueue 统计 Claim 次数。
type countingQueue struct {
	*memqueue.Queue
	claims atomic.Int64
}

func (q *countingQueue) Claim(ctx context.Context, holder string, ttl time.Duration) (*workqueue.Lease, error) {
	q.claims.Add(1)
	return q.Queue.Claim(ctx, holder, ttl)
}

func TestIdleGateBoundsPollingAndWakesOnEnqueue(t *testing.T) {
	clk := clock.Real{}
	q := &countingQueue{Queue: memqueue.New(clk)}
	store := &session.Store{Log: memlog.New(), IDs: ids.Random(), Clock: clk}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "echo", Version: "1", Model: "echo/any"})
	gw := model.NewGateway()
	gw.Register("echo", echo.Provider{})
	gate := &IdleGate{Ready: q.Ready(), Min: 20 * time.Millisecond, Max: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range 20 {
		w := New(Config{ID: string(rune('a' + i)), Store: store, Queue: q, Agents: agents, Model: gw,
			Catalog: &capability.Catalog{Local: capability.NewRegistry()}, Idle: gate, IdleWait: 10 * time.Millisecond})
		go w.Run(ctx)
	}

	// 20 个空闲 Worker 在 500ms 内：不加门控约 1000 次认领（各自每 10ms 一次）；加门控后只有一个在退避轮询。
	time.Sleep(500 * time.Millisecond)
	if n := q.claims.Load(); n > 15 {
		t.Fatalf("%d claims while idle; the gate should allow about one poller", n)
	}

	// 入队信号立即唤醒，而不是等退避结束。
	st := &session.State{SessionID: "s1"}
	if err := store.Commit(ctx, st, sessionCreated()); err != nil {
		t.Fatal(err)
	}
	before := q.claims.Load()
	start := time.Now()
	if err := q.Enqueue(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	for q.claims.Load() == before {
		if time.Since(start) > 80*time.Millisecond {
			t.Fatal("enqueue did not wake the idle poller")
		}
		time.Sleep(time.Millisecond)
	}
}

func sessionCreated() *v1.Event {
	return &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}}
}
