package main

import (
	"context"
	"net/http"
	"time"

	"persona/mcpserver"
	"persona/store"
	"xinli/delivery"
	"xinli/recovery"
)

// 生产构建没有实现或注册测试参数；装配接缝让验收使用同一对象图。
type testHooks interface {
	Now() time.Time
	Books(store.Store) store.Store
	Notifier(mcpserver.Notifier) mcpserver.Notifier
	Consumer(recovery.Consumer) recovery.Consumer
	Delivery(delivery.Store) delivery.Store
	Push(delivery.Push) delivery.Push
	Ticks(context.Context) <-chan time.Time
	Diagnostics() http.Handler
}

func (c config) now() time.Time {
	if c.test != nil {
		return c.test.Now()
	}
	return time.Now()
}

func (c config) ticks(ctx context.Context) <-chan time.Time {
	if c.test != nil {
		return c.test.Ticks(ctx)
	}
	return nil
}

func (c config) retryInterval() time.Duration {
	if c.test != nil {
		return 100 * time.Millisecond
	}
	return 0
}
