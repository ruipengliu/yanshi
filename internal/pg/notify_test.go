package pg

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitAboveUsesNotifiedValue(t *testing.T) {
	n := NewNotifier(nil, nil)
	var queries atomic.Int32
	head := atomic.Uint64{}
	query := func(context.Context) (uint64, error) { queries.Add(1); return head.Load(), nil }

	result := make(chan uint64, 1)
	go func() {
		v, err := n.WaitAbove(context.Background(), TopicEvents, "s1", 5, query)
		if err != nil {
			t.Error(err)
		}
		result <- v
	}()
	for queries.Load() == 0 { // 等初次查询完成（订阅已建立）
		time.Sleep(time.Millisecond)
	}
	n.signal(string(TopicEvents), "s1:4") // 值未超过 after：继续等待，不查询
	n.signal(string(TopicEvents), "s2:9") // 其他键：不唤醒
	time.Sleep(20 * time.Millisecond)
	head.Store(7)
	n.signal(string(TopicEvents), "s1:7")
	select {
	case v := <-result:
		if v != 7 {
			t.Fatalf("got %d, want 7", v)
		}
	case <-time.After(time.Second):
		t.Fatal("not woken by notification")
	}
	if q := queries.Load(); q != 1 {
		t.Fatalf("queried %d times, want only the initial query", q)
	}
}

func TestWaitAboveQueriesOnPlainPayload(t *testing.T) {
	n := NewNotifier(nil, nil)
	var queries atomic.Int32
	head := atomic.Uint64{}
	query := func(context.Context) (uint64, error) { queries.Add(1); return head.Load(), nil }
	result := make(chan uint64, 1)
	go func() {
		v, _ := n.WaitAbove(context.Background(), TopicEvents, "s1", 0, query)
		result <- v
	}()
	for queries.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	head.Store(3)
	n.signal(string(TopicEvents), "s1") // 旧格式（不带值）：回退到查询
	select {
	case v := <-result:
		if v != 3 || queries.Load() != 2 {
			t.Fatalf("v = %d, queries = %d", v, queries.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("not woken")
	}
}

func TestWaitAboveSkipsQueryWhenCached(t *testing.T) {
	n := NewNotifier(nil, nil)
	n.signal(string(TopicEvents), "s1:9") // 无订阅者时也记录
	v, err := n.WaitAbove(context.Background(), TopicEvents, "s1", 3, func(context.Context) (uint64, error) {
		t.Fatal("queried although a newer value was cached")
		return 0, nil
	})
	if err != nil || v != 9 {
		t.Fatalf("v = %d, err = %v", v, err)
	}
	n.invalidate() // 连接失效：缓存作废，必须查询
	queried := false
	v, _ = n.WaitAbove(context.Background(), TopicEvents, "s1", 3, func(context.Context) (uint64, error) { queried = true; return 10, nil })
	if !queried || v != 10 {
		t.Fatalf("after reconnect: queried = %v, v = %d", queried, v)
	}
}

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := newLRU(2)
	c.put("a", 1)
	c.put("b", 2)
	if v, ok := c.get("a"); !ok || v != 1 {
		t.Fatalf("a = %d, %v", v, ok)
	}
	c.put("c", 3) // b 最久未用
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	for k, want := range map[string]uint64{"a": 1, "c": 3} {
		if v, ok := c.get(k); !ok || v != want {
			t.Fatalf("%s = %d, %v", k, v, ok)
		}
	}
	c.put("a", 4)
	if v, _ := c.get("a"); v != 4 || c.order.Len() != 2 {
		t.Fatalf("update: a = %d, len %d", v, c.order.Len())
	}
}

func TestChannelNames(t *testing.T) {
	var none *Notifier
	if got := none.Channel(TopicEvents, "s1"); got != "yanshi_events" {
		t.Fatalf("nil notifier channel = %q", got)
	}
	if got := NewNotifier(nil, nil).Channel(TopicInbox, "n1"); got != "yanshi_inbox" {
		t.Fatalf("one shard: channel = %q, want the topic itself", got)
	}
	n := NewNotifier(nil, nil).WithShards(16)
	seen := map[string]bool{}
	for i := range 200 {
		ch := n.Channel(TopicEvents, fmt.Sprintf("s%d", i))
		if ch != n.Channel(TopicEvents, fmt.Sprintf("s%d", i)) {
			t.Fatal("channel is not stable")
		}
		seen[ch] = true
	}
	if len(seen) != 16 {
		t.Fatalf("200 keys over 16 shards used %d channels", len(seen))
	}
}

// 校准按轮转顺序唤醒订阅者重查：每个键至多每 CalibrateInterval 一次，且每秒不超过 CalibrateBudget 个键；
// 被校准唤醒的等待者以查询为准，而不是缓存。
func TestCalibrationIsRateLimitedAndForcesQuery(t *testing.T) {
	n := NewNotifier(nil, nil)
	n.CalibrateInterval, n.CalibrateBudget = time.Second, 20
	const keys = 100
	var queries atomic.Int32
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range keys {
		key := fmt.Sprintf("s%d", i)
		n.signal(string(TopicEvents), key+":5") // 缓存值不超过 after：不查询就会一直等
		go func() {
			_, _ = n.WaitAbove(ctx, TopicEvents, key, 5, func(context.Context) (uint64, error) {
				queries.Add(1)
				return 5, nil
			})
			done <- struct{}{}
		}()
	}
	for {
		n.mu.Lock()
		subscribed := len(n.subs)
		n.mu.Unlock()
		if subscribed == keys {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if q := queries.Load(); q != 0 {
		t.Fatalf("waiters with cached values queried %d times before any calibration", q)
	}
	// 一秒的节拍：100 个键、每键每秒一次的目标被每秒 20 个的额度截住。
	for range 10 {
		n.calibrate(100 * time.Millisecond)
	}
	waitFor(t, func() bool { return queries.Load() == 20 })
	time.Sleep(20 * time.Millisecond)
	if q := queries.Load(); q != 20 {
		t.Fatalf("one second of calibration queried %d keys, budget 20", q)
	}
	// 再过四秒，每个键都被校准过一次。
	for range 40 {
		n.calibrate(100 * time.Millisecond)
	}
	waitFor(t, func() bool { return queries.Load() == keys })
	cancel()
	for range keys {
		<-done
	}
}

// 连接重建后全部键分批重查（RecheckRate），不同时查询。
func TestReconnectRechecksInBatches(t *testing.T) {
	n := NewNotifier(nil, nil)
	n.CalibrateInterval, n.RecheckRate = 0, 30 // 关闭校准，只看重查
	const keys = 100
	var queries atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range keys {
		key := fmt.Sprintf("s%d", i)
		go func() {
			_, _ = n.WaitAbove(ctx, TopicEvents, key, 5, func(context.Context) (uint64, error) {
				queries.Add(1)
				return 5, nil
			})
		}()
	}
	waitFor(t, func() bool { return queries.Load() == keys }) // 初次检查
	n.invalidate()
	n.onConnected(false)
	n.onConnected(true)
	n.calibrate(100 * time.Millisecond)
	waitFor(t, func() bool { return queries.Load() == keys+3 })
	time.Sleep(20 * time.Millisecond)
	if q := queries.Load(); q != keys+3 {
		t.Fatalf("first batch after reconnect queried %d keys, want 3", q-keys)
	}
	for range 40 {
		n.calibrate(100 * time.Millisecond)
	}
	waitFor(t, func() bool { return queries.Load() == 2*keys })
	if g := n.Generation(); g != 2 {
		t.Fatalf("generation %d, want 2", g)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
