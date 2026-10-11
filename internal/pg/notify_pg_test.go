package pg_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
)

// TestShardedChannelsListenOnDemand：分片的频道只在有订阅者时 LISTEN（ADR-0028）；监听循环阻塞在等待中时
// 新增的订阅同样生效；没有订阅者的分片上的通知不会送到本进程。
func TestShardedChannelsListenOnDemand(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := pg.NewNotifier(pool, nil).WithShards(8)
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// 三个分布在不同分片上的键。
	var keys []string
	used := map[string]bool{}
	for i := 0; len(keys) < 3; i++ {
		k := fmt.Sprintf("s%d", i)
		if ch := n.Channel(pg.TopicEvents, k); !used[ch] {
			used[ch] = true
			keys = append(keys, k)
		}
	}
	notify := func(key string, v int) {
		if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, n.Channel(pg.TopicEvents, key), fmt.Sprintf("%s:%d", key, v)); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range keys[:2] {
		got := make(chan uint64, 1)
		go func() {
			v, _ := n.WaitAbove(ctx, pg.TopicEvents, key, 0, func(context.Context) (uint64, error) { return 0, nil })
			got <- v
		}()
		// LISTEN 是异步生效的：反复通知直到收到。
		deadline := time.After(5 * time.Second)
	wait:
		for {
			notify(key, 1)
			select {
			case v := <-got:
				if v != 1 {
					t.Fatalf("%s: got %d", key, v)
				}
				break wait
			case <-time.After(50 * time.Millisecond):
			case <-deadline:
				t.Fatalf("%s: notification never arrived", key)
			}
		}
		if v, ok := n.Latest(pg.TopicEvents, key); !ok || v != 1 {
			t.Fatalf("%s: latest = %d, %v", key, v, ok)
		}
	}
	// 第三个键所在的分片没有订阅者：本进程没有 LISTEN 它的频道。
	notify(keys[2], 7)
	notify(keys[0], 2) // 同一连接上的通知按序到达：收到它之后，上一条若会送达也已送达
	deadline := time.Now().Add(5 * time.Second)
	for v, _ := n.Latest(pg.TopicEvents, keys[0]); v != 2; v, _ = n.Latest(pg.TopicEvents, keys[0]) {
		if time.Now().After(deadline) {
			t.Fatal("notification on a listened channel never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v, ok := n.Latest(pg.TopicEvents, keys[2]); ok {
		t.Fatalf("received %d on a channel without subscribers", v)
	}
}

// TestWaiterConvergesAfterListenerLoss：LISTEN 连接断开期间发出的通知丢失（末条漏发），等待者没有各自的轮询，
// 靠重连后的分批重查收敛。
func TestWaiterConvergesAfterListenerLoss(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := pg.NewNotifier(pool, nil)
	n.Heartbeat, n.HeartbeatTimeout = 100*time.Millisecond, 500*time.Millisecond
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	var head atomic.Uint64
	got := make(chan uint64, 1)
	go func() {
		v, _ := n.WaitAbove(ctx, pg.TopicEvents, "s1", 0, func(context.Context) (uint64, error) { return head.Load(), nil })
		got <- v
	}()
	deadline := time.Now().Add(5 * time.Second)
	for n.Generation() == 0 || n.ListenerPID() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("notifier never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	gen := n.Generation()
	time.Sleep(800 * time.Millisecond) // 若干次心跳：健康的连接不应被重建
	if g := n.Generation(); g != gen {
		t.Fatalf("healthy listener reconnected: generation %d → %d", gen, g)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, int32(n.ListenerPID())); err != nil {
		t.Fatal(err)
	}
	head.Store(1)
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, 's1:1')`, n.Channel(pg.TopicEvents, "s1")); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != 1 {
			t.Fatalf("got %d", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter never converged after the listener was lost")
	}
	// 等待者可能先由校准收敛，重连随后完成：等它，而不是要求二者同时发生。
	for deadline := time.Now().Add(10 * time.Second); n.Generation() <= gen; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("generation %d did not advance past %d", n.Generation(), gen)
		}
	}
}
