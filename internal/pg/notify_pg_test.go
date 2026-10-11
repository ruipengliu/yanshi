package pg_test

import (
	"context"
	"fmt"
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
