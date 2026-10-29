package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"yanshi/internal/pg"
)

// readiness 是就绪检查（docs/design/deployment.md §6）：数据库可达、本进程的 LISTEN 连接已建立、迁移已全部应用。
// 滚动发布时流量不会打到尚未监听通知的进程。/healthz 只表示进程活着（存活检查），不查依赖。
type readiness struct {
	st    *storage
	roles map[string]bool

	mu sync.Mutex
	// migrated 是最近一次确认迁移已全部应用的时刻：迁移只在发布时变化，每分钟复查一次即可。
	migrated time.Time
}

func (r *readiness) check(ctx context.Context) error {
	if len(r.st.shardConnections) > 0 {
		return r.checkShards(ctx)
	}
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
	if r.st.routingDigest != "" {
		w.Header().Set("X-Yanshi-Routing", r.st.routingDigest)
	}
	if len(r.st.shardConnections) > 0 {
		rows, ready := r.shardReport(req.Context())
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "shards": rows})
		return
	}
	if err := r.check(req.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// 各角色可以处理健康分片的独立工作；库 0 只限制实际依赖控制数据的请求。
func (r *readiness) shardReport(ctx context.Context) ([]shardStatus, bool) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	parts := r.st.shardConnections
	rows := make([]shardStatus, len(parts))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(4, len(parts)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				rows[i] = parts[i].probe(ctx)
			}
		}()
	}
	for i := range parts {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	ready := false
	for _, status := range rows {
		usable := status.SQL && status.Migrations && status.Identity
		if r.roles["gateway"] || len(r.roles) == 0 {
			usable = usable && status.Listen
		}
		ready = ready || usable
	}
	return rows, ready
}

func (r *readiness) checkShards(ctx context.Context) error {
	_, ready := r.shardReport(ctx)
	if !ready {
		return fmt.Errorf("no serviceable shard")
	}
	return nil
}
