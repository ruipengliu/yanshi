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
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/artifact/fsblob"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/artifact/s3blob"
	"yanshi/internal/auth"
	"yanshi/internal/call"
	"yanshi/internal/capability"
	"yanshi/internal/channel"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/feed"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/live"
	"yanshi/internal/live/peer"
	"yanshi/internal/mcpcap"
	"yanshi/internal/memory"
	"yanshi/internal/memory/pgmemory"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/model/bench"
	"yanshi/internal/model/echo"
	"yanshi/internal/model/openaicompat"
	"yanshi/internal/model/script"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/node/wsgateway"
	"yanshi/internal/notify"
	"yanshi/internal/notify/pgnotify"
	"yanshi/internal/pg"
	"yanshi/internal/presence"
	"yanshi/internal/presence/pgpresence"
	"yanshi/internal/realtime"
	"yanshi/internal/realtime/volc"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	sandboxdocker "yanshi/internal/sandbox/docker"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/session/pgsnapshot"
	"yanshi/internal/usage"
	"yanshi/internal/usage/pgusage"
	"yanshi/internal/webui"
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

	memories     memory.Store
	grants       memory.Grants
	snapshots    session.Snapshots
	index        lifecycle.Index
	deletions    lifecycle.Deletions
	janitorQueue workqueue.Queue
	usage        usage.Store
	presence     presence.Store
	push         notify.Registry
}

// openStorage 按 kind 创建存储。postgres 模式下多个 serve 进程可共享同一数据库水平扩展。
// notifyShards 是 PostgreSQL 通知的分片数（ADR-0028），所有进程须一致。migrate 为 false 时不执行迁移，只检查
// 迁移都已应用（生产中由独立的 yanshi migrate 任务执行）。
func openStorage(ctx context.Context, kind, dsn string, notifyShards int, migrate bool, clk clock.Clock, logger *slog.Logger) (backends, func(), error) {
	switch kind {
	case "memory":
		return backends{memlog.New(), memqueue.New(clk), node.NewMemDirectory(clk), node.NewMemInbox(),
			memqueue.New(clk), nodesdk.NewMemLedger(), sandbox.NewMemActivity(), artifact.NewMemMeta(),
			memory.NewMemStore(), memory.NewMemGrants(), session.NewMemSnapshots(),
			lifecycle.NewMemIndex(), lifecycle.NewMemDeletions(), memqueue.New(clk), usage.NewMem(), presence.NewMem(), notify.NewMemRegistry()}, func() {}, nil
	case "postgres":
		pool, err := pg.Open(ctx, dsn, "")
		if err != nil {
			return backends{}, nil, fmt.Errorf("connect postgres: %w", err)
		}
		if err := migrateOrCheck(ctx, pool, migrate); err != nil {
			pool.Close()
			return backends{}, nil, err
		}
		metrics.RegisterPool(func() (int32, int32, int32, int64, int64, time.Duration) {
			s := pool.Stat()
			return s.AcquiredConns(), s.TotalConns(), s.MaxConns(), s.AcquireCount(), s.EmptyAcquireCount(), s.AcquireDuration()
		})
		n := pg.NewNotifier(pool, logger).WithShards(notifyShards)
		nctx, cancel := context.WithCancel(ctx)
		go n.Run(nctx)
		b := backends{pglog.New(pool, n), pgqueue.New(pool, clk, pgqueue.Sessions).WithNotifier(n), pgnode.NewDirectory(pool, clk), pgnode.NewInbox(pool, n),
			pgqueue.New(pool, clk, pgqueue.Sandboxes).WithNotifier(n), pgsandbox.Ledger{Pool: pool}, pgsandbox.Activity{Pool: pool},
			pgartifact.Meta{Pool: pool},
			pgmemory.Store{Pool: pool}, pgmemory.Grants{Pool: pool}, pgsnapshot.Store{Pool: pool},
			pglifecycle.Index{Pool: pool}, pglifecycle.Deletions{Pool: pool, Notifier: n}, pgqueue.New(pool, clk, pgqueue.Janitor), pgusage.Store{Pool: pool},
			pgpresence.Store{Pool: pool, Notifier: n}, pgnotify.Registry{Pool: pool}}
		return b, func() { cancel(); pool.Close() }, nil
	}
	return backends{}, nil, fmt.Errorf("unknown storage %q (memory | postgres)", kind)
}

// migrateOrCheck 在开发时直接迁移；生产中迁移由独立任务完成，这里只确认没有遗漏：版本不一致的进程不应启动。
func migrateOrCheck(ctx context.Context, pool *pgxpool.Pool, migrate bool) error {
	if migrate {
		if err := pg.Migrate(ctx, pool); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		return nil
	}
	pending, err := pg.Pending(ctx, pool)
	if err != nil {
		return fmt.Errorf("check migrations: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("database is missing migrations %v: run yanshi migrate first", pending)
	}
	return nil
}

// migrateCmd 应用 PostgreSQL 迁移后退出。多个进程同时启动时由迁移锁串行化；生产部署中作为发布前的独立任务运行，
// 使大表上的迁移（如 CREATE INDEX CONCURRENTLY，pg.NoTransaction）不必在每个 serve 进程启动时执行。
func migrateCmd(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dsn := fs.String("pg-dsn", envOr("YANSHI_PG_DSN", devPGDSN), "PostgreSQL 地址")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pg.Open(ctx, *dsn, "")
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()
	pending, err := pg.Pending(ctx, pool)
	if err != nil {
		return err
	}
	if err := pg.Migrate(ctx, pool); err != nil {
		return err
	}
	fmt.Printf("applied %d migration(s)\n", len(pending))
	return nil
}

// defaultModelLimits 是调用真实模型提供商的默认约束（model.Limits）：每次调用 10 分钟时限，暂时性失败重试 2 次；
// 不限并发与速率。提供商有账号级的并发或速率上限时，用 serve -model-concurrency / -model-rps 配置。
var defaultModelLimits = model.Limits{Timeout: 10 * time.Minute, Retries: 2, Backoff: time.Second, MaxBackoff: 30 * time.Second}

// gateway 按环境变量配置模型供应商；真实提供商经 model.Guard 施加 limits（每个提供商各自计数）：
//
//	echo   始终可用
//	script 始终可用：按输入中的命令调用工具（调试控制台与测试，internal/model/script）
//	ark       ARK_API_KEY（可选 ARK_BASE_URL）
//	tokenhub  TOKENHUB_API_KEY（可选 TOKENHUB_BASE_URL），腾讯云 TokenHub 的 OpenAI 兼容接口
//	local  YANSHI_LOCAL_BASE_URL（可选 YANSHI_LOCAL_API_KEY），私有化 OpenAI 兼容推理服务
func gateway(logger *slog.Logger, limits model.Limits) *model.Gateway {
	gw := model.NewGateway()
	gw.Register("echo", echo.Provider{})
	gw.Register("bench", bench.Provider{})
	gw.Register("script", script.Provider{})
	register := func(name, base, key string) {
		gw.Register(name, &model.Guard{Name: name, Provider: &openaicompat.Provider{BaseURL: base, APIKey: key}, Limits: limits})
		logger.Info("model provider configured", "provider", name, "base_url", base,
			"max_concurrency", limits.MaxConcurrency, "rps", limits.RPS, "timeout", limits.Timeout)
	}
	if key := os.Getenv("ARK_API_KEY"); key != "" {
		register("ark", envOr("ARK_BASE_URL", openaicompat.ArkBaseURL), key)
	}
	if key := os.Getenv("TOKENHUB_API_KEY"); key != "" {
		register("tokenhub", envOr("TOKENHUB_BASE_URL", openaicompat.TokenHubBaseURL), key)
	}
	if base := os.Getenv("YANSHI_LOCAL_BASE_URL"); base != "" {
		register("local", base, os.Getenv("YANSHI_LOCAL_API_KEY"))
	}
	return gw
}

// realtimeGateway 按环境变量配置实时语音模型（Call，docs/design/m3-call.md）：
//
//	volc  VOLC_SPEECH_API_KEY（可选 VOLC_SPEECH_URL），豆包实时语音 Seeduplex
func realtimeGateway(logger *slog.Logger) *realtime.Gateway {
	gw := realtime.NewGateway()
	if key := os.Getenv("VOLC_SPEECH_API_KEY"); key != "" {
		gw.Register("volc", &volc.Provider{APIKey: key, URL: os.Getenv("VOLC_SPEECH_URL")})
		logger.Info("realtime provider configured", "provider", "volc")
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
	interactiveWorkers := fs.Int("interactive-workers", -1, "其中只认领交互工作的 Worker 数（ADR-0029）：长任务占满其余 Worker 时问答仍有 Worker 可用；默认为 Worker 数的四分之一")
	drainTimeout := fs.Duration("drain-timeout", 20*time.Second, "优雅停机时等待进行中的这一步（如模型调用）完成的上限，之后移交 Run；应小于编排系统的终止宽限期")
	storage := fs.String("storage", "memory", "存储：memory | postgres")
	dsn := fs.String("pg-dsn", envOr("YANSHI_PG_DSN", devPGDSN), "PostgreSQL 地址（storage=postgres）")
	notifyShards := fs.Int("notify-shards", 1, "PostgreSQL 通知的分片数（ADR-0028）：进程只 LISTEN 有订阅者的分片频道；所有进程须一致")
	migrate := fs.Bool("migrate", true, "启动时应用 PostgreSQL 迁移；生产中由 yanshi migrate 任务执行，serve 置 false 时只检查迁移都已应用")
	sandboxKind := fs.String("sandbox", "none", "代码沙箱：none | docker")
	sandboxImage := fs.String("sandbox-image", "yanshi-sandbox:dev", "沙箱镜像（make sandbox-image 构建）")
	sandboxRuntime := fs.String("sandbox-runtime", "", "沙箱容器运行时，如 runsc（gVisor）")
	mcpDir := fs.String("mcp", "mcp", "远程 MCP Server 登记目录（docs/design/m5-mcp.md）；不存在时不启用")
	embedModel := fs.String("embedding-model", os.Getenv("YANSHI_EMBEDDING_MODEL"), "Memory 检索用的嵌入模型（provider/model，如 ark/doubao-embedding-vision）；为空时按文本相似度检索")
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
	pricing := fs.String("pricing", "pricing.yaml", "价格表（docs/design/m4-quota-usage.md §2）；不存在时用量只计 token、不折算金额")
	moderationKind := fs.String("moderation", "mock", "内容安全提供商（docs/design/m4-moderation.md）：mock（关键词，仅开发测试；上线前须接入真实提供商）| none")
	moderationTerms := fs.String("moderation-terms", "", "mock 提供商的关键词文件（每行一个）；默认只含测试标记词")
	usageRetention := fs.Duration("usage-retention", 400*24*time.Hour, "用量记录的保留期")
	modelLimits := defaultModelLimits
	fs.DurationVar(&modelLimits.Timeout, "model-timeout", modelLimits.Timeout, "一次模型调用的时限（不含排队与重试等待）")
	fs.IntVar(&modelLimits.Retries, "model-retries", modelLimits.Retries, "模型调用暂时性失败（限流、服务端错误、连接中断）的重试次数；已输出内容后不重试")
	fs.IntVar(&modelLimits.MaxConcurrency, "model-concurrency", 0, "本进程对每个模型提供商的并发调用上限，0 表示不限；排队时交互调用优先（ADR-0029）")
	fs.Float64Var(&modelLimits.RPS, "model-rps", 0, "本进程对每个模型提供商每秒发起调用的上限，0 表示不限")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	agents, err := agentdef.LoadDir(*agentsDir)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	// 发布配置（docs/design/m4-agent-rollout.md）：启动时必须合法；运行中定期重新加载，回滚无需重启。
	releasesPath := filepath.Join(*agentsDir, agentdef.ReleasesFile)
	rel, releasesLoaded, err := agents.LoadReleases(releasesPath)
	if err != nil {
		return fmt.Errorf("load releases: %w", err)
	}
	agents.SetReleases(rel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clk := clock.Real{}
	verifier, nodeAuth, lines, err := authenticator(*authMode, *addr, *blDir, *audience, clk)
	if err != nil {
		return err
	}
	b, closeStorage, err := openStorage(ctx, *storage, *dsn, *notifyShards, *migrate, clk, logger)
	if err != nil {
		return err
	}
	defer closeStorage()
	store := &session.Store{Log: b.log, IDs: ids.Random(), Clock: clk, Snapshots: b.snapshots, Deletions: b.deletions}
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
	arts := &artifact.Service{Meta: b.artifactMeta, Blobs: blobs, IDs: ids.Random(), Clock: clk, Deletions: b.deletions}
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
	gw := gateway(logger, modelLimits)
	dir := b.dir
	hub := &node.Hub{
		Dir: dir, Inbox: b.inbox, Store: store, Queue: queue,
		Auth: nodeAuth, Waker: node.LogWaker{Logger: logger}, Push: b.push, Clock: clk, Logger: logger,
	}
	// 提醒（docs/design/m3-duplex-channel.md §9）：推送通道目前是日志桩，真实通道见上线检查清单。
	notifier := &notify.Notifier{Registry: b.push, Presence: b.presence, Pusher: notify.LogPusher{Logger: logger}, Clock: clk, Logger: logger}
	// Memory（docs/design/m4-memory-grant.md）：Agent 通过能力读写，Run 开始时召回。
	mems := &memory.Service{Store: b.memories, Grants: b.grants, Deletions: b.deletions, Clock: clk, IDs: ids.Random(),
		HealthAllowed: healthAllowed(*authMode, lines)}
	if *embedModel != "" {
		mems.Embedder, mems.EmbedModel = gw, *embedModel
	}
	catalog := &capability.Catalog{
		Local: capability.NewRegistry(append([]capability.Capability{capability.ClockNow(clk)}, memory.Capabilities(mems)...)...),
		Nodes: dir, DefaultTimeout: 30 * time.Minute, NodeCacheTTL: 10 * time.Second, Clock: clk,
	}
	mcpServers, err := mcpcap.LoadDir(*mcpDir)
	if err != nil {
		return fmt.Errorf("load MCP servers: %w", err)
	}
	if len(mcpServers) > 0 {
		conn := &mcpcap.Connector{Servers: mcpServers, Artifacts: arts, Logger: logger}
		defer conn.Close()
		catalog.MCP = conn
		for _, s := range mcpServers {
			logger.Info("MCP server registered", "name", s.Name, "business_line", s.BusinessLine, "url", s.URL)
		}
	}
	hub.Deletions = b.deletions
	router := &sandbox.Router{Hub: hub, Queue: b.sandboxQueue}
	meter, quotas, err := metering(*pricing, lines, agents, b, logger)
	if err != nil {
		return err
	}
	moderator, err := moderatorFor(*moderationKind, *moderationTerms, logger)
	if err != nil {
		return err
	}
	svc := &service.Service{Store: store, Queue: queue, Agents: agents, Nodes: router,
		Index: b.index, Deletions: b.deletions, Janitor: b.janitorQueue, Quotas: quotas, Moderator: moderator, Artifacts: arts}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		agents.WatchReleases(ctx, releasesPath, releasesLoaded, 10*time.Second, logger)
	}()
	// 本进程同一池的 Worker 共享空闲门控：空闲时只有一个 Worker 轮询队列，入队信号到达时立即认领。
	// 交互 Worker 与其余 Worker 各用一个门控（workqueue.Fanout）。
	idle, interactiveIdle := &workqueue.IdleGate{}, &workqueue.IdleGate{}
	if s, ok := queue.(workqueue.Signaler); ok {
		ready := workqueue.Fanout(ctx, s.Ready(), 2)
		idle.Ready, interactiveIdle.Ready = ready[0], ready[1]
	}
	if *interactiveWorkers < 0 {
		*interactiveWorkers = *workers / 4
	}
	if *workers > 0 && *interactiveWorkers >= *workers {
		return fmt.Errorf("-interactive-workers must leave at least one worker for long runs (got %d of %d)", *interactiveWorkers, *workers)
	}
	for i := range *workers {
		// 前 interactiveWorkers 个只认领交互工作：长任务移交给其余 Worker（ADR-0029）。
		pool, gate := workqueue.Pool{}, idle
		if i < *interactiveWorkers {
			pool.MinPriority, gate = workqueue.Interactive, interactiveIdle
		}
		w := runtime.New(runtime.Config{
			ID: fmt.Sprintf("%s-worker-%d", proc, i), Store: store, Queue: queue, Agents: agents,
			Model: gw, Catalog: catalog, Dispatch: router, Artifacts: arts, Memory: mems, Live: bus, LiveEndpoint: liveEndpoint, Logger: logger,
			LeaseTTL: *leaseTTL, Heartbeat: *leaseTTL / 3, Idle: gate, Meter: meter, Quotas: quotas, Moderator: moderator,
			Notify: notifier, Pool: pool, DrainTimeout: *drainTimeout,
		})
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(ctx) }()
	}

	var provider sandbox.Provider
	switch *sandboxKind {
	case "none":
	case "docker":
		catalog.Sandbox = &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID}
		provider = &sandboxdocker.Provider{Image: *sandboxImage, Runtime: *sandboxRuntime}
		sbxIdle := &workqueue.IdleGate{}
		if s, ok := b.sandboxQueue.(workqueue.Signaler); ok {
			sbxIdle.Ready = s.Ready()
		}
		for i := range *controllers {
			c := &sandbox.Controller{
				ID: fmt.Sprintf("%s-sandbox-%d", proc, i), Queue: b.sandboxQueue, Hub: hub, Provider: provider,
				Activity: b.activity, Ledger: b.ledger, Artifacts: arts, Clock: clk, Logger: logger,
				Lifecycle: &lifecycle.Guard{Index: b.index, Deletions: b.deletions}, Idle: sbxIdle, Meter: meter,
				LeaseTTL: *leaseTTL, Heartbeat: *leaseTTL / 3,
			}
			wg.Add(1)
			go func() { defer wg.Done(); c.Run(ctx) }()
		}
		logger.Info("sandbox enabled", "provider", "docker", "image", *sandboxImage, "runtime", *sandboxRuntime)
	default:
		return fmt.Errorf("unknown sandbox %q (none | docker)", *sandboxKind)
	}

	// Janitor 回收已关闭与已删除 Session 的资源，并执行各业务线的保留策略（docs/design/m2-session-lifecycle.md）。
	retention := map[string]lifecycle.Retention{}
	for _, bl := range lines {
		retention[bl.Name] = bl.Retention
	}
	jan := &janitor.Janitor{
		ID: proc + "-janitor", Queue: b.janitorQueue, Service: svc, Store: store, Sessions: queue, SandboxQueue: b.sandboxQueue,
		Inbox: b.inbox, Nodes: dir, Sandbox: provider, Activity: b.activity, Ledger: b.ledger, Artifacts: arts,
		Memory: b.memories, Index: b.index, Deletions: b.deletions, Retention: retention, Clock: clk, Logger: logger, LeaseTTL: *leaseTTL,
		Usage: b.usage, UsageRetention: *usageRetention, Presence: b.presence,
	}
	wg.Add(1)
	go func() { defer wg.Done(); jan.Run(ctx) }()

	mux := http.NewServeMux()
	// 同一条连接兼任 Node 与会话客户端（ADR-0024）；/v1/nodes/connect 是早期路径，保留兼容。
	calls := &call.Manager{Service: svc, Realtime: realtimeGateway(logger), Meter: meter, Logger: logger,
		Feed: feed.Source{Log: b.log, Live: bus, Deletions: b.deletions}}
	connGW := &wsgateway.Gateway{Hub: hub, Channel: &channel.Handler{Service: svc, Live: bus,
		Presence: &presence.Service{Store: b.presence, Deletions: b.deletions, Clock: clk}, Push: b.push, Calls: calls, Clock: clk, Logger: logger}, Logger: logger}
	mux.Handle("/v1/connect", connGW)
	mux.Handle("/v1/nodes/connect", connGW)
	// 网页通话页与调试控制台（开发与演示，docs/design/m3-call.md §8、docs/design/console.md）。
	mux.Handle("/call", webui.Handler())
	mux.Handle("/call/", webui.Handler())
	mux.Handle("/console", webui.Handler())
	mux.Handle("/console/", webui.Handler())
	mux.Handle("/", (&httpapi.Server{Auth: verifier, Service: svc, Live: bus, Nodes: dir, Artifacts: arts, Memory: mems, Usage: b.usage, Push: b.push, Logger: logger}).Handler())
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
	logger.Info("yanshi serving", "addr", *addr, "auth", *authMode, "process", proc, "workers", *workers, "interactive_workers", *interactiveWorkers,
		"storage", *storage, "blob", *blobKind, "live_endpoint", liveEndpoint)
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
// 同时返回业务线配置，供保留策略使用（auth=none 时没有业务线配置，不执行保留策略）。
func authenticator(mode, addr, dir, audience string, clk clock.Clock) (auth.Verifier, node.Authenticator, []auth.BusinessLine, error) {
	switch mode {
	case "none":
		if !loopback(addr) {
			return nil, nil, nil, fmt.Errorf("-auth none only allows a loopback -addr (got %s); use -auth jwt", addr)
		}
		return auth.Insecure{}, node.InsecureDevAuth{}, nil, nil
	case "jwt":
		lines, err := auth.LoadDir(dir)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(lines) == 0 {
			return nil, nil, nil, fmt.Errorf("no business lines in %s; run yanshi keygen for a dev key", dir)
		}
		v, err := auth.NewJWT(audience, clk, lines...)
		if err != nil {
			return nil, nil, nil, err
		}
		return v, node.TokenAuth{Verifier: v}, lines, nil
	}
	return nil, nil, nil, fmt.Errorf("unknown auth %q (none | jwt)", mode)
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

// metering 加载价格表并汇总各业务线的配额（docs/design/m4-quota-usage.md）。任一业务线配置了配额时，
// 所有 AgentDef 引用的模型都必须有价格：没有价格的模型等于不受配额约束。
func metering(path string, lines []auth.BusinessLine, agents *agentdef.Registry, b backends, logger *slog.Logger) (*usage.Meter, *usage.Quotas, error) {
	prices, err := usage.LoadPriceList(path)
	if errors.Is(err, os.ErrNotExist) {
		prices, err = nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load price list: %w", err)
	}
	limits := map[string]usage.Limits{}
	for _, bl := range lines {
		if bl.Quota.Enabled() {
			limits[bl.Name] = bl.Quota
		}
	}
	var missing []string
	for _, d := range agents.All() {
		for _, m := range []string{d.Model, d.Context.SummaryModel, d.Call.Model} {
			if m != "" && !prices.Has(m) && !slices.Contains(missing, m) {
				missing = append(missing, m)
			}
		}
	}
	if len(missing) > 0 {
		if len(limits) > 0 {
			return nil, nil, fmt.Errorf("quotas are configured but %s has no price for %v", path, missing)
		}
		logger.Warn("models without price: usage is recorded at zero cost", "models", missing)
	}
	if len(limits) == 0 {
		return &usage.Meter{Store: b.usage, Prices: prices, Deletions: b.deletions, Logger: logger}, nil, nil
	}
	// 配额检查读缓存的已用金额（5 秒）；计量经同一个缓存写入，本进程的花费立即计入（usage.SpentCache）。
	spent := usage.NewSpentCache(b.usage, 5*time.Second, clock.Real{})
	return &usage.Meter{Store: spent, Prices: prices, Deletions: b.deletions, Logger: logger}, &usage.Quotas{Store: spent, Limits: limits}, nil
}

// moderatorFor 选择内容安全提供商。目前只有模拟实现：它没有真实的识别能力，启动时打印警告并置指标，
// 上线前须接入真实提供商（docs/launch-checklist.md）。
func moderatorFor(kind, termsPath string, logger *slog.Logger) (moderation.Moderator, error) {
	switch kind {
	case "none":
		logger.Warn("content moderation is disabled (-moderation none): not allowed in production")
		return nil, nil
	case "mock":
		m := moderation.Mock{Terms: moderation.MockTerms}
		if termsPath != "" {
			terms, err := moderation.LoadTerms(termsPath)
			if err != nil {
				return nil, fmt.Errorf("load moderation terms: %w", err)
			}
			m.Terms = terms
		}
		metrics.ModerationMock.Set(1)
		logger.Warn("content moderation uses the MOCK provider (keyword list): replace it with a real provider before launch", "terms", len(m.Terms))
		return m, nil
	}
	return nil, fmt.Errorf("unknown moderation provider %q (mock | none)", kind)
}

// healthAllowed 返回各业务线是否允许健康信息（ADR-0022）：业务线配置 memory.health 声明已取得单独同意。
// -auth none 是只监听回环地址的开发模式，没有业务线配置，一律允许，便于开发与演示。
func healthAllowed(authMode string, lines []auth.BusinessLine) func(string) bool {
	if authMode == "none" {
		return func(string) bool { return true }
	}
	enabled := map[string]bool{}
	for _, bl := range lines {
		enabled[bl.Name] = bl.Memory.Health
	}
	return func(bl string) bool { return enabled[bl] }
}
