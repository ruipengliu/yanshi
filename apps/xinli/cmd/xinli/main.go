// xinli 将 Persona 与应用角色装配为独立于内核的进程。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"persona/command"
	"persona/fixture"
	"persona/mcpserver"
	"persona/pg"
	"persona/query"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
	"xinli/account"
	"xinli/bff"
	"xinli/capture"
	"xinli/delivery"
	"xinli/devui"
	"xinli/kernel"
	"xinli/orchestrator"
	appPG "xinli/pg"
	"xinli/recovery"
	"xinli/task"
	taskmem "xinli/task/memstore"
	taskpg "xinli/task/pgstore"
)

type registration struct {
	Name         string            `json:"name"`
	BusinessLine string            `json:"business_line"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers,omitempty"`
	Timeout      string            `json:"timeout"`
	Tools        struct {
		NoApproval []string `json:"no_approval"`
	} `json:"tools"`
	CiteCaptures    bool `json:"cite_captures"`
	AttestsSpeakers bool `json:"attests_speakers"`
	Acks            bool `json:"acks"`
}

type ready struct {
	APIURL        string         `json:"api_url,omitempty"`
	HealthURL     string         `json:"health_url,omitempty"`
	Roles         []string       `json:"roles,omitempty"`
	MCPURL        string         `json:"mcp_url"`
	FixtureURL    string         `json:"fixture_url"`
	PageURL       string         `json:"page_url"`
	Registrations []registration `json:"registrations"`
}

func registrations(lines []string, endpoint string) []registration {
	source := mcpserver.XinliRegistration()
	out := make([]registration, 0, len(lines))
	for _, bl := range lines {
		r := registration{Name: source.Name, BusinessLine: bl, URL: endpoint, Timeout: source.Timeout.String(), CiteCaptures: source.CiteCaptures, Acks: source.Acks, AttestsSpeakers: source.AttestsSpeakers}
		r.Tools.NoApproval = append([]string(nil), source.NoApproval...)
		out = append(out, r)
	}
	return out
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if handled, err := testCommand(ctx, args, stdin, stdout); handled {
		return err
	}
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		return err
	}
	if cfg.mode == "migrate" {
		return migrateDatabases(ctx, cfg)
	}
	return serve(ctx, cfg, stdin, stdout, stderr)
}

// serve 与 dev 共用对象图与生命周期，只有路由和默认配置不同。
func serve(ctx context.Context, cfg config, stdin io.Reader, stdout, stderr io.Writer) error {
	var logHandler slog.Handler = slog.NewTextHandler(stderr, nil)
	if cfg.test != nil {
		logHandler = cfg.test.LogHandler(logHandler)
	}
	logger := slog.New(logHandler)
	life, cancelLife := context.WithCancel(context.Background())
	defer cancelLife()
	books, closeBooks, personaReady, err := openStore(ctx, cfg.store, cfg.personaDSN, cfg.mode == "dev", cfg.now)
	if err != nil {
		return err
	}
	defer closeBooks()
	if cfg.test != nil {
		books = cfg.test.Books(books)
	}
	checks := []func(context.Context) error{personaReady}
	var orch *orchestrator.Orchestrator
	var client *kernel.Client
	var accounts *account.Service
	if cfg.roles.orchestrator || cfg.roles.bff || cfg.mode == "dev" && cfg.identityProvider != nil {
		tasks, closeTasks, taskReady, err := openTasks(ctx, cfg.store, cfg.appDSN, cfg.personaDSN, cfg.mode == "dev")
		if err != nil {
			return err
		}
		defer closeTasks()
		checks = append(checks, taskReady)
		client = &kernel.Client{BaseURL: cfg.kernelURL, Tokens: cfg.tokens}
		accounts = &account.Service{Store: tasks.(account.Store), Provider: cfg.identityProvider, AllowUnregistered: cfg.mode == "dev"}
		if cfg.roles.orchestrator {
			accountWorker := &account.Worker{Accounts: accounts, Tasks: tasks, Persona: books, Kernel: client}
			orch = orchestrator.New(life, books, client, orchestrator.Options{Logger: logger, Tasks: tasks, Accounts: accounts, AccountWorker: accountWorker, Now: cfg.now, RetryInterval: cfg.retryInterval()})
			// 应用独占推送登记；不向内核 Connection 注册推送设备。P4 只有确定性 fake。
			sink := &delivery.Fake{}
			consumer := &delivery.Consumer{Progress: tasks.(recovery.Consumer), Store: tasks.(delivery.Store), Kernel: delivery.Source{Client: client}, Accounts: tasks.(account.Enumerator), Terminal: orch.ScanTasks, Now: cfg.now}
			worker := &delivery.Worker{Store: tasks.(delivery.Store), Accounts: accounts.Store, Persona: &query.Service{Store: books, Now: cfg.now}, Kernel: client, Push: sink, Channel: sink, Now: cfg.now}
			if cfg.test != nil {
				consumer.Progress = cfg.test.Consumer(consumer.Progress)
				consumer.Store = cfg.test.Delivery(consumer.Store)
				worker.Push = cfg.test.Push(worker.Push)
			}
			stopDelivery := delivery.Start(life, cfg.lines, consumer, worker, func(f delivery.Failure) {
				logger.Warn("application delivery retry", "stage", f.Stage, "shard", f.Shard, "error_class", f.Category,
					"business_line", f.BusinessLine, "phase", f.Phase, "session_id", f.SessionID, "run_id", f.RunID, "call_id", f.CallID, "envelope_id", f.EnvelopeID)
			})
			defer stopDelivery()
		}
		defer func() {
			cancelLife()
			if orch != nil {
				_ = orch.Shutdown(context.Background())
			}
		}()
	}
	mux := routes(cfg, books, orch, client, accounts)
	probes := http.NewServeMux()
	if cfg.test != nil {
		probes.Handle("/debug/day", cfg.test.Diagnostics())
	}
	probes.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	probes.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		probe, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, check := range checks {
			if check(probe) != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	var servers []*http.Server
	serveErr := make(chan error, 2)
	defer func() {
		for _, server := range servers {
			_ = server.Close()
		}
	}()
	listen := func(addr string, handler http.Handler) (string, error) {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return "", errors.New("xinli: listen failed")
		}
		host, port, err := net.SplitHostPort(listener.Addr().String())
		if err != nil {
			_ = listener.Close()
			return "", errors.New("xinli: invalid listener address")
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
		servers = append(servers, server)
		go func() {
			serveErr <- server.Serve(listener)
		}()
		return "http://" + net.JoinHostPort(host, port), nil
	}
	announcement := ready{}
	if cfg.roles.persona || cfg.roles.bff || cfg.mode == "dev" {
		base, err := listen(cfg.addr, mux)
		if err != nil {
			return err
		}
		if cfg.mode == "serve" {
			announcement.APIURL = base
		}
		if cfg.roles.persona {
			announcement.MCPURL = base + "/mcp"
			announcement.Registrations = registrations(cfg.lines, announcement.MCPURL)
			for i := range announcement.Registrations {
				announcement.Registrations[i].Headers = cfg.headers[announcement.Registrations[i].BusinessLine]
			}
		}
		if cfg.mode == "dev" {
			announcement.FixtureURL, announcement.PageURL = base, base+"/"
		}
	}
	if cfg.mode == "serve" {
		base, err := listen(cfg.healthAddr, probes)
		if err != nil {
			return err
		}
		announcement.HealthURL, announcement.Roles = base, cfg.roles.names()
	}
	if cfg.mcpDir != "" {
		if err := os.MkdirAll(cfg.mcpDir, 0700); err != nil {
			return errors.New("xinli: cannot create mcp-dir")
		}
		for _, r := range announcement.Registrations {
			data, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				return errors.New("xinli: cannot encode registration")
			}
			path := filepath.Join(cfg.mcpDir, "xinli-"+r.BusinessLine+".yaml")
			// dev 的随机凭据只交给父进程与私有登记文件，不能落入普通日志或公开文件。
			if err := writeRegistration(path, append(data, '\n')); err != nil {
				return err
			}
		}
	}
	if orch != nil {
		orch.StartDeletions()
		orch.StartTasks(cfg.ticks(life))
		orch.StartExpirations(cfg.ticks(life))
		if cfg.autoSchedule {
			orch.StartSchedules(cfg.ticks(life))
		}
		stopPurge := startPurger(life, store.Purger{Store: books, Now: cfg.now}, logger)
		defer stopPurge()
	}
	// stdout 是父进程的机器协议；开发登记包含本次进程的短期随机凭据。
	if err := json.NewEncoder(stdout).Encode(announcement); err != nil {
		return errors.New("xinli: cannot write readiness")
	}
	eof := make(chan struct{})
	if cfg.exitOnEOF {
		go func() {
			_, _ = io.Copy(io.Discard, stdin)
			close(eof)
		}()
	}
	var result error
	select {
	case <-ctx.Done():
	case <-eof:
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.New("xinli: HTTP server failed")
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
			result = errors.New("xinli: HTTP shutdown timed out")
		}
	}
	if orch != nil && orch.Shutdown(shutdown) != nil {
		cancelLife()
		result = errors.New("xinli: orchestration shutdown timed out")
	}
	return result
}

func routes(cfg config, books store.Store, orch *orchestrator.Orchestrator, client *kernel.Client, services ...*account.Service) http.Handler {
	var accounts *account.Service
	if len(services) > 0 {
		accounts = services[0]
	}
	mux := http.NewServeMux()
	if cfg.roles.persona {
		opts := mcpserver.Options{Credentials: cfg.credentials, InsecureDev: cfg.insecureMCP, AllowUnmanaged: cfg.mode == "dev" && orch == nil, Now: cfg.now, AuthNow: time.Now}
		// typed nil 不能放进接口，否则 Persona 独立进程会在接受事实后调用 nil 编排器。
		if orch != nil {
			opts.Notifier = orch
		}
		if cfg.test != nil {
			opts.Notifier = cfg.test.Notifier(opts.Notifier)
		}
		mux.Handle("/mcp", mcpserver.Handler(books, opts))
		mux.Handle("/v1/query/", query.Handler(&query.Service{Store: books, Now: cfg.now}, cfg.credentials, nil))
		mux.Handle("/v1/commands/", command.Handler(&command.Service{Store: books, Now: cfg.now}, cfg.credentials, nil))
	}
	if cfg.roles.bff {
		mountBFF(mux, cfg, books, client, accounts)
	}
	if accounts != nil && (cfg.roles.bff || cfg.mode == "dev") {
		mux.Handle("/v1/auth/", accounts.Handler())
		mux.Handle("/v1/captures/", capture.Handler(books, accounts))
	}
	if cfg.mode != "dev" {
		return mux
	}
	fixtureOpts := fixture.Options{}
	if cfg.fixtureClock != nil {
		fixtureOpts.Now = cfg.now
		mux.Handle("POST /v1/fixture/clock", cfg.fixtureClock)
	}
	uiOpts := devui.Options{Store: books}
	if orch != nil {
		fixtureOpts.Notifier = orch
		uiOpts.DaySessions, uiOpts.Kernel, uiOpts.Notifier = orch, client, orch
	}
	mux.Handle("/v1/fixture/", fixture.Handler(books, fixtureOpts))
	mux.HandleFunc("POST /v1/fixture/{businessLine}/{endUser}/week_review", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if orch == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"orchestrator disabled: set -kernel-url"}`)
			return
		}
		sid, err := orch.Week(r.Context(), r.PathValue("businessLine"), r.PathValue("endUser"))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"week review failed"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": sid})
	})
	mux.Handle("/", devui.Handler(devui.Config{BusinessLines: cfg.lines, ConsoleURL: cfg.consoleURL}, uiOpts))
	if accounts != nil {
		return accounts.DevelopmentHandler(mux)
	}
	return mux
}

// 同进程与远程 Persona 共用查询与命令契约；产品请求始终先经过应用登录态。
func mountBFF(mux *http.ServeMux, cfg config, books store.Store, client *kernel.Client, accounts *account.Service) {
	var reader query.Reader = &query.Service{Store: books, Now: cfg.now}
	var writer command.Writer = &command.Service{Store: books, Now: cfg.now}
	if cfg.personaURL != "" {
		token := func(ctx context.Context, line string) (string, error) {
			for i := len(cfg.credentials) - 1; i >= 0; i-- {
				credential := cfg.credentials[i]
				if credential.BusinessLine == line && time.Now().Before(credential.ExpiresAt) {
					return credential.Token, nil
				}
			}
			return "", errors.New("xinli: Persona credential unavailable")
		}
		reader = &query.Client{BaseURL: cfg.personaURL, Token: token}
		writer = &command.Client{BaseURL: cfg.personaURL, Token: token}
	}
	var tasks task.Reader
	var inbox delivery.Store
	if accounts != nil {
		tasks, _ = accounts.Store.(task.Reader)
		inbox, _ = accounts.Store.(delivery.Store)
	}
	mux.Handle("/v1/", bff.Handler(bff.Options{Accounts: accounts, Persona: reader, Commands: writer, Kernel: client, Tasks: tasks, Inbox: inbox, Now: cfg.now}))
}

func startPurger(ctx context.Context, purger store.Purger, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if err := purger.Scan(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("xinli: owner purge failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// dev 可自动迁移；serve 只验证迁移已完成，不要求运行角色持有 DDL 权限。
func openStore(ctx context.Context, kind, dsn string, migrate bool, now ...func() time.Time) (store.Store, func(), func(context.Context) error, error) {
	switch kind {
	case "memory":
		return memstore.New(now...), func() {}, func(context.Context) error {
			return nil
		}, nil
	case "pg":
		if strings.TrimSpace(dsn) == "" {
			return nil, nil, nil, errors.New("xinli: pg requires -persona-dsn or YANSHI_PERSONA_PG_DSN (a separate database)")
		}
		startup, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		pool, err := pg.Open(startup, dsn, "")
		if err != nil {
			// 驱动错误可能包含连接信息，不能把 DSN 写入启动日志。
			return nil, nil, nil, errors.New("xinli: cannot open Persona database")
		}
		check := pg.Ready
		if migrate {
			check = pg.Migrate
		}
		if err := check(startup, pool); err != nil {
			pool.Close()
			return nil, nil, nil, errors.New("xinli: Persona migration failed")
		}
		s := pgstore.New(pool, now...)
		return s, s.Close, func(ctx context.Context) error {
			return pg.Ready(ctx, pool)
		}, nil
	default:
		return nil, nil, nil, errors.New("xinli: store must be memory or pg")
	}
}

// PG 编排不能确认到内存任务库；显式配置应用 DSN，避免重启后事实已确认而任务丢失。
func openTasks(ctx context.Context, kind, dsn, personaDSN string, migrate bool) (task.Store, func(), func(context.Context) error, error) {
	if kind == "memory" {
		return taskmem.New(), func() {}, func(context.Context) error {
			return nil
		}, nil
	}
	if kind != "pg" {
		return nil, nil, nil, errors.New("xinli: invalid task storage")
	}
	if dsn == "" {
		return nil, nil, nil, errors.New("xinli: pg orchestration requires -app-dsn")
	}
	if err := validateDatabases(personaDSN, dsn); err != nil {
		return nil, nil, nil, err
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := appPG.Open(startup, dsn, "")
	if err != nil {
		return nil, nil, nil, errors.New("xinli: cannot open application database")
	}
	check := appPG.Ready
	if migrate {
		check = appPG.Migrate
	}
	if err := check(startup, pool); err != nil {
		pool.Close()
		return nil, nil, nil, errors.New("xinli: application migration failed")
	}
	return taskpg.New(pool), pool.Close, func(ctx context.Context) error {
		return appPG.Ready(ctx, pool)
	}, nil
}

// 临时文件从创建起就是 0600；原子替换避免重启时短暂暴露旧文件权限或半份登记。
func writeRegistration(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".xinli-registration-*")
	if err != nil {
		return errors.New("xinli: cannot create registration")
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return errors.New("xinli: cannot write registration")
	}
	if err := file.Close(); err != nil {
		return errors.New("xinli: cannot close registration")
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return errors.New("xinli: cannot replace registration")
	}
	return nil
}
