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
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/model"
	"yanshi/internal/model/echo"
	"yanshi/internal/model/openaicompat"
	"yanshi/internal/node"
	"yanshi/internal/node/wsgateway"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

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

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	agentsDir := fs.String("agents", "agents", "AgentDef 目录")
	workers := fs.Int("workers", 4, "Worker 数量")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	agents, err := agentdef.LoadDir(*agentsDir)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}

	clk := clock.Real{}
	store := &session.Store{Log: memlog.New(), IDs: ids.Random(), Clock: clk}
	queue := memqueue.New(clk)
	bus := live.NewMemBus()
	gw := gateway(logger)
	dir := node.NewMemDirectory(clk)
	hub := &node.Hub{
		Dir: dir, Inbox: node.NewMemInbox(), Store: store, Queue: queue,
		Auth: node.InsecureDevAuth{}, Waker: node.LogWaker{Logger: logger}, Logger: logger,
	}
	catalog := &capability.Catalog{
		Local: capability.NewRegistry(capability.ClockNow(clk)), Nodes: dir, DefaultTimeout: 30 * time.Minute,
	}
	svc := &service.Service{Store: store, Queue: queue, Agents: agents, Nodes: hub}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	logger.Info("yanshi serving", "addr", *addr, "workers", *workers, "storage", "memory")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	wg.Wait()
	return nil
}
