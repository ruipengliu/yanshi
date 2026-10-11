package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"yanshi/internal/pg"
)

// readiness 是就绪检查（docs/design/deployment.md §6）：数据库可达、本进程的 LISTEN 连接已建立、迁移已全部应用。
// 滚动发布时流量不会打到尚未监听通知的进程。/healthz 只表示进程活着（存活检查），不查依赖。
type readiness struct {
	st *storage

	mu sync.Mutex
	// migrated 是最近一次确认迁移已全部应用的时刻：迁移只在发布时变化，每分钟复查一次即可。
	migrated time.Time
}

func (r *readiness) check(ctx context.Context) error {
	if r.st.pool == nil {
		return nil // 进程内存储：没有外部依赖
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := r.st.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database unreachable: %w", err)
	}
	if !r.st.notifier.Ready() {
		return fmt.Errorf("notification listener not connected")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.migrated) < time.Minute {
		return nil
	}
	pending, err := pg.Pending(ctx, r.st.pool)
	if err != nil {
		return fmt.Errorf("check migrations: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d migrations not applied", len(pending))
	}
	r.migrated = time.Now()
	return nil
}

func (r *readiness) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if err := r.check(req.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
