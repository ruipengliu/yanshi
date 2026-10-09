package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/artifact/fsblob"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/artifact/s3blob"
	"yanshi/internal/auth"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/live/peer"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/model/bench"
	"yanshi/internal/model/echo"
	"yanshi/internal/model/openaicompat"
	"yanshi/internal/node"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/node/wsgateway"
	"yanshi/internal/pg"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	sandboxdocker "yanshi/internal/sandbox/docker"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
	"yanshi/internal/workqueue/pgqueue"
	"yanshi/sdk/nodesdk"
)

// devPGDSN 对应 docker-compose.yml 中的开发数据库。
const devPGDSN = "postgres://yanshi:yanshi@127.0.0.1:54329/yanshi?sslmode=disable"

type backends struct {
	log   eventlog.Log
	queue workqueue.Queue
	dir   node.Directory
	inbox node.Inbox

	sandboxQueue workqueue.Queue
	ledger       nodesdk.Ledger
	activity     sandbox.Activity
	artifactMeta artifact.MetaStore
}

// openStorage 按 kind 创建存储。postgres 模式下多个 serve 进程可共享同一数据库水平扩展。
func openStorage(ctx context.Context, kind, dsn string, clk clock.Clock, logger *slog.Logger) (backends, func(), error) {
	switch kind {
	case "memory":
		return backends{memlog.New(), memqueue.New(clk), node.NewMemDirectory(clk), node.NewMemInbox(),
			memqueue.New(clk), nodesdk.NewMemLedger(), sandbox.NewMemActivity(), artifact.NewMemMeta()}, func() {}, nil
	case "postgres":
		pool, err := pg.Open(ctx, dsn, "")
		if err != nil {
			return backends{}, nil, fmt.Errorf("connect postgres: %w", err)
		}
		if err := pg.Migrate(ctx, pool); err != nil {
			pool.Close()
			return backends{}, nil, fmt.Errorf("migrate: %w", err)
		}
		metrics.RegisterPool(func() (int32, int32, int32, int64, int64, time.Duration) {
			s := pool.Stat()
			return s.AcquiredConns(), s.TotalConns(), s.MaxConns(), s.AcquireCount(), s.EmptyAcquireCount(), s.AcquireDuration()
		})
		n := pg.NewNotifier(pool, logger)
		nctx, cancel := context.WithCancel(ctx)
		go n.Run(nctx)
		b := backends{pglog.New(pool, n), pgqueue.New(pool, clk, pgqueue.Sessions).WithNotifier(n), pgnode.NewDirectory(pool, clk), pgnode.NewInbox(pool, n),
			pgqueue.New(pool, clk, pgqueue.Sandboxes), pgsandbox.Ledger{Pool: pool}, pgsandbox.Activity{Pool: pool},
			pgartifact.Meta{Pool: pool}}
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
	gw.Register("bench", bench.Provider{})
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
	sandboxKind := fs.String("sandbox", "none", "代码沙箱：none | docker")
	sandboxImage := fs.String("sandbox-image", "yanshi-sandbox:dev", "沙箱镜像（make sandbox-image 构建）")
	sandboxRuntime := fs.String("sandbox-runtime", "", "沙箱容器运行时，如 runsc（gVisor）")
	controllers := fs.Int("sandbox-controllers", 2, "沙箱控制器数量")
	blobKind := fs.String("blob", "fs", "工件内容存储：fs | s3")
	blobDir := fs.String("blob-dir", "data/artifacts", "工件目录（blob=fs）")
	s3Endpoint := fs.String("s3-endpoint", envOr("YANSHI_S3_ENDPOINT", "127.0.0.1:58333"), "S3 地址 host:port（blob=s3）")
	s3Bucket := fs.String("s3-bucket", envOr("YANSHI_S3_BUCKET", "yanshi-artifacts"), "S3 桶")
	s3SSL := fs.Bool("s3-ssl", false, "S3 使用 HTTPS")
	peerAddr := fs.String("peer-addr", "", "内部监听地址：其他进程从这里拉取实时增量（ADR-0013），Prometheus 从 /metrics 抓取指标；storage=postgres 时默认 127.0.0.1:0，off 表示关闭")
	peerAdvertise := fs.String("peer-advertise", "", "其他进程访问本进程内部地址所用的 URL，如 http://$POD_IP:7070；默认取监听地址")
	leaseTTL := fs.Duration("lease-ttl", 10*time.Second, "工作队列租约时长：进程崩溃后，其进行中的 Run 约在此时长后被接管（心跳为其 1/3）")
	authMode := fs.String("auth", "none", "鉴权：none（信任自报身份，仅允许监听回环地址）| jwt（业务线签发的令牌，docs/design/auth.md）")
	blDir := fs.String("businesslines", "businesslines", "业务线公钥配置目录（auth=jwt；yanshi keygen 生成开发配置）")
	audience := fs.String("auth-audience", "yanshi", "本部署的标识，令牌的 aud 须包含它")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	agents, err := agentdef.LoadDir(*agentsDir)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clk := clock.Real{}
	verifier, nodeAuth, err := authenticator(*authMode, *addr, *blDir, *audience, clk)
	if err != nil {
		return err
	}
	b, closeStorage, err := openStorage(ctx, *storage, *dsn, clk, logger)
	if err != nil {
		return err
	}
	defer closeStorage()
	store := &session.Store{Log: b.log, IDs: ids.Random(), Clock: clk}
	var blobs artifact.BlobStore
	switch *blobKind {
	case "fs":
		blobs = fsblob.Store{Dir: *blobDir}
	case "s3":
		s3, err := s3blob.New(ctx, s3blob.Config{
			Endpoint: *s3Endpoint, Bucket: *s3Bucket, UseSSL: *s3SSL,
			AccessKey: envOr("YANSHI_S3_ACCESS_KEY", "yanshi"), SecretKey: envOr("YANSHI_S3_SECRET_KEY", "yanshi-dev-secret"),
		})
		if err != nil {
			return fmt.Errorf("connect s3: %w", err)
		}
		blobs = s3
	default:
		return fmt.Errorf("unknown blob store %q (fs | s3)", *blobKind)
	}
	arts := &artifact.Service{Meta: b.artifactMeta, Blobs: blobs, IDs: ids.Random(), Clock: clk}
	queue := b.queue
	// 进程 ID 使 Worker 与控制器的 ID 在多进程部署中唯一，日志中的 worker_id 可以对应到进程。
	proc := ids.Random()()[:8]
	var bus live.Bus = live.NewMemBus()
	var liveEndpoint string
	if *peerAddr == "" && *storage == "postgres" {
		*peerAddr = "127.0.0.1:0"
	}
	if *peerAddr != "" && *peerAddr != "off" {
		var stopPeer func()
		bus, liveEndpoint, stopPeer, err = startPeer(*peerAddr, *peerAdvertise, bus.(*live.MemBus), b.log, logger)
		if err != nil {
			return err
		}
		defer stopPeer()
	}
	gw := gateway(logger)
	dir := b.dir
	hub := &node.Hub{
		Dir: dir, Inbox: b.inbox, Store: store, Queue: queue,
		Auth: nodeAuth, Waker: node.LogWaker{Logger: logger}, Logger: logger,
	}
	catalog := &capability.Catalog{
		Local: capability.NewRegistry(capability.ClockNow(clk)), Nodes: dir, DefaultTimeout: 30 * time.Minute,
	}
	router := &sandbox.Router{Hub: hub, Queue: b.sandboxQueue}
	svc := &service.Service{Store: store, Queue: queue, Agents: agents, Nodes: router}

	var wg sync.WaitGroup
	// 本进程的 Worker 共享空闲门控：空闲时只有一个 Worker 轮询队列，入队信号到达时立即认领。
	idle := &runtime.IdleGate{}
	if s, ok := queue.(workqueue.Signaler); ok {
		idle.Ready = s.Ready()
	}
	for i := range *workers {
		w := runtime.New(runtime.Config{
			ID: fmt.Sprintf("%s-worker-%d", proc, i), Store: store, Queue: queue, Agents: agents,
			Model: gw, Catalog: catalog, Dispatch: router, Artifacts: arts, Live: bus, LiveEndpoint: liveEndpoint, Logger: logger,
			LeaseTTL: *leaseTTL, Heartbeat: *leaseTTL / 3, Idle: idle,
		})
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(ctx) }()
	}

	switch *sandboxKind {
	case "none":
	case "docker":
		catalog.Sandbox = &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID}
		provider := &sandboxdocker.Provider{Image: *sandboxImage, Runtime: *sandboxRuntime}
		for i := range *controllers {
			c := &sandbox.Controller{
				ID: fmt.Sprintf("%s-sandbox-%d", proc, i), Queue: b.sandboxQueue, Hub: hub, Provider: provider,
				Activity: b.activity, Ledger: b.ledger, Artifacts: arts, Clock: clk, Logger: logger,
				LeaseTTL: *leaseTTL, Heartbeat: *leaseTTL / 3,
			}
			wg.Add(1)
			go func() { defer wg.Done(); c.Run(ctx) }()
		}
		logger.Info("sandbox enabled", "provider", "docker", "image", *sandboxImage, "runtime", *sandboxRuntime)
	default:
		return fmt.Errorf("unknown sandbox %q (none | docker)", *sandboxKind)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/nodes/connect", &wsgateway.Gateway{Hub: hub, Logger: logger})
	mux.Handle("/", (&httpapi.Server{Auth: verifier, Service: svc, Live: bus, Nodes: dir, Artifacts: arts, Logger: logger}).Handler())
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
	logger.Info("yanshi serving", "addr", *addr, "auth", *authMode, "process", proc, "workers", *workers, "storage", *storage, "blob", *blobKind, "live_endpoint", liveEndpoint)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	wg.Wait()
	return nil
}

// startPeer 在内部地址上提供本进程的实时增量，并返回跨进程的 live.Bus 与本进程的地址（ADR-0013）。
// 令牌取自 YANSHI_PEER_TOKEN；内部地址不应对外暴露。
func startPeer(addr, advertise string, local *live.MemBus, log eventlog.Log, logger *slog.Logger) (live.Bus, string, func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", nil, fmt.Errorf("peer listen: %w", err)
	}
	if advertise == "" {
		host, _, _ := net.SplitHostPort(ln.Addr().String())
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
			ln.Close()
			return nil, "", nil, fmt.Errorf("peer-addr %s listens on all interfaces; set -peer-advertise", addr)
		}
		advertise = "http://" + ln.Addr().String()
	}
	token := os.Getenv("YANSHI_PEER_TOKEN")
	mux := http.NewServeMux()
	mux.Handle(peer.Path, peer.Handler{Local: local, Token: token})
	mux.Handle("/metrics", metrics.Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("peer server stopped", "err", err)
		}
	}()
	bus := &peer.Bus{Local: local, Log: log, Self: advertise, Token: token, Logger: logger}
	return bus, advertise, func() { _ = srv.Close() }, nil
}

// authenticator 按 -auth 构造 API 与 Node 接入的鉴权（docs/design/auth.md §5）。
// none 信任自报身份，只允许监听回环地址：误部署到网络上时直接启动失败，而不是不设防地运行。
func authenticator(mode, addr, dir, audience string, clk clock.Clock) (auth.Verifier, node.Authenticator, error) {
	switch mode {
	case "none":
		if !loopback(addr) {
			return nil, nil, fmt.Errorf("-auth none only allows a loopback -addr (got %s); use -auth jwt", addr)
		}
		return auth.Insecure{}, node.InsecureDevAuth{}, nil
	case "jwt":
		lines, err := auth.LoadDir(dir)
		if err != nil {
			return nil, nil, err
		}
		if len(lines) == 0 {
			return nil, nil, fmt.Errorf("no business lines in %s; run yanshi keygen for a dev key", dir)
		}
		v, err := auth.NewJWT(audience, clk, lines...)
		if err != nil {
			return nil, nil, err
		}
		return v, node.TokenAuth{Verifier: v}, nil
	}
	return nil, nil, fmt.Errorf("unknown auth %q (none | jwt)", mode)
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
