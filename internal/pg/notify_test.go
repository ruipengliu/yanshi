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
	n.broadcast() // 连接重建：缓存失效，必须查询
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
