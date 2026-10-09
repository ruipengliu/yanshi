package pg

import (
	"context"
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
		v, err := n.WaitAbove(context.Background(), ChannelEvents, "s1", 5, query)
		if err != nil {
			t.Error(err)
		}
		result <- v
	}()
	for queries.Load() == 0 { // 等初次查询完成（订阅已建立）
		time.Sleep(time.Millisecond)
	}
	n.signal(ChannelEvents, "s1:4") // 值未超过 after：继续等待，不查询
	n.signal(ChannelEvents, "s2:9") // 其他键：不唤醒
	time.Sleep(20 * time.Millisecond)
	head.Store(7)
	n.signal(ChannelEvents, "s1:7")
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
		v, _ := n.WaitAbove(context.Background(), ChannelEvents, "s1", 0, query)
		result <- v
	}()
	for queries.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	head.Store(3)
	n.signal(ChannelEvents, "s1") // 旧格式（不带值）：回退到查询
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
	n.signal(ChannelEvents, "s1:9") // 无订阅者时也记录
	v, err := n.WaitAbove(context.Background(), ChannelEvents, "s1", 3, func(context.Context) (uint64, error) {
		t.Fatal("queried although a newer value was cached")
		return 0, nil
	})
	if err != nil || v != 9 {
		t.Fatalf("v = %d, err = %v", v, err)
	}
	n.broadcast() // 连接重建：缓存失效，必须查询
	queried := false
	v, _ = n.WaitAbove(context.Background(), ChannelEvents, "s1", 3, func(context.Context) (uint64, error) { queried = true; return 10, nil })
	if !queried || v != 10 {
		t.Fatalf("after reconnect: queried = %v, v = %d", queried, v)
	}
}
