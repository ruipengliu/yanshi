package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/model"
	"yanshi/internal/model/echo"
	"yanshi/internal/model/openaicompat"
	"yanshi/internal/node"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/node/wsgateway"
	"yanshi/internal/pg"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
	"yanshi/internal/workqueue/pgqueue"
)

// devPGDSN 对应 docker-compose.yml 中的开发数据库。
const devPGDSN = "postgres://yanshi:yanshi@127.0.0.1:54329/yanshi?sslmode=disable"

type backends struct {
	log   eventlog.Log
	queue workqueue.Queue
	dir   node.Directory
	inbox node.Inbox
}

// openStorage 按 kind 创建存储。postgres 模式下多个 serve 进程可共享同一数据库水平扩展。
func openStorage(ctx context.Context, kind, dsn string, clk clock.Clock, logger *slog.Logger) (backends, func(), error) {
	switch kind {
	case "memory":
		return backends{memlog.New(), memqueue.New(clk), node.NewMemDirectory(clk), node.NewMemInbox()}, func() {}, nil
	case "postgres":
		pool, err := pg.Open(ctx, dsn, "")
		if err != nil {
			return backends{}, nil, fmt.Errorf("connect postgres: %w", err)
		}
		if err := pg.Migrate(ctx, pool); err != nil {
			pool.Close()
			return backends{}, nil, fmt.Errorf("migrate: %w", err)
		}
		n := pg.NewNotifier(pool, logger)
		nctx, cancel := context.WithCancel(ctx)
		go n.Run(nctx)
		b := backends{pglog.New(pool, n), pgqueue.New(pool, clk), pgnode.NewDirectory(pool, clk), pgnode.NewInbox(pool, n)}
		return b, func() { cancel(); pool.Close() }, nil
	}
	return backends{}, nil, fmt.Errorf("unknown storage %q (memory | postgres)", kind)
}

// gateway 按环境变量配置模型供应商：
//
//	echo   始终可用
//	ark    ARK_API_KEY（可选 ARK_BASE_URL）
//	local  YANSHI_LOCAL_BASE_URL（可选 YANSHI_LOCAL_API_KEY），私有化 OpenAI 兼容推理服务
func gateway(logger *slog.Logger) *model.Gateway {
	gw := model.NewGateway()
	gw.Register("echo", echo.Provider{})
	if key := os.Getenv("ARK_API_KEY"); key != "" {
		base := os.Getenv("ARK_BASE_URL")
		if base == "" {
			base = openaicompat.ArkBaseURL
		}
		gw.Register("ark", &openaicompat.Provider{BaseURL: base, APIKey: key})
		logger.Info("model provider configured", "provider", "ark", "base_url", base)
	}
	if base := os.Getenv("YANSHI_LOCAL_BASE_URL"); base != "" {
		gw.Register("local", &openaicompat.Provider{BaseURL: base, APIKey: os.Getenv("YANSHI_LOCAL_API_KEY")})
		logger.Info("model provider configured", "provider", "local", "base_url", base)
	}
	return gw
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	agentsDir := fs.String("agents", "agents", "AgentDef 目录")
	workers := fs.Int("workers", 4, "Worker 数量")
	storage := fs.String("storage", "memory", "存储：memory | postgres")
	dsn := fs.String("pg-dsn", envOr("YANSHI_PG_DSN", devPGDSN), "PostgreSQL 地址（storage=postgres）")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	agents, err := agentdef.LoadDir(*agentsDir)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clk := clock.Real{}
	b, closeStorage, err := openStorage(ctx, *storage, *dsn, clk, logger)
	if err != nil {
		return err
	}
	defer closeStorage()
	store := &session.Store{Log: b.log, IDs: ids.Random(), Clock: clk}
	queue := b.queue
	// LiveBus 是进程内的：多进程部署时 token 增量只送达连在同一进程的客户端，已提交事件不受影响。
	bus := live.NewMemBus()
	gw := gateway(logger)
	dir := b.dir
	hub := &node.Hub{
		Dir: dir, Inbox: b.inbox, Store: store, Queue: queue,
		Auth: node.InsecureDevAuth{}, Waker: node.LogWaker{Logger: logger}, Logger: logger,
	}
	catalog := &capability.Catalog{
		Local: capability.NewRegistry(capability.ClockNow(clk)), Nodes: dir, DefaultTimeout: 30 * time.Minute,
	}
	svc := &service.Service{Store: store, Queue: queue, Agents: agents, Nodes: hub}

	var wg sync.WaitGroup
	for i := range *workers {
		w := runtime.New(runtime.Config{
			ID: fmt.Sprintf("worker-%d", i), Store: store, Queue: queue, Agents: agents,
			Model: gw, Catalog: catalog, Dispatch: hub, Live: bus, Logger: logger,
			LeaseTTL: 30 * time.Second, Heartbeat: 10 * time.Second,
		})
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(ctx) }()
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/nodes/connect", &wsgateway.Gateway{Hub: hub, Logger: logger})
	mux.Handle("/", (&httpapi.Server{Service: svc, Live: bus, Nodes: dir, Logger: logger}).Handler())
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("yanshi serving", "addr", *addr, "workers", *workers, "storage", *storage)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	wg.Wait()
	return nil
}
