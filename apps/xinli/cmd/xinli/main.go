// xinli dev 将夹具、MCP 与开发页面装配为一个独立进程。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"persona/fixture"
	"persona/mcpserver"
	"persona/pg"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
	"xinli/devui"
	"xinli/kernel"
	"xinli/orchestrator"
	appPG "xinli/pg"
	"xinli/task"
	taskmem "xinli/task/memstore"
	taskpg "xinli/task/pgstore"

	"github.com/jackc/pgx/v5"
)

type registration struct {
	Name         string `json:"name"`
	BusinessLine string `json:"business_line"`
	URL          string `json:"url"`
	Timeout      string `json:"timeout"`
	Tools        struct {
		NoApproval []string `json:"no_approval"`
	} `json:"tools"`
	CiteCaptures    bool `json:"cite_captures"`
	AttestsSpeakers bool `json:"attests_speakers"`
	Acks            bool `json:"acks"`
}

type ready struct {
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
	if len(args) == 0 || args[0] != "dev" {
		return errors.New("usage: xinli dev -business-lines <lines>")
	}
	flags := flag.NewFlagSet("xinli dev", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", "127.0.0.1:0", "监听地址")
	businessLines := flags.String("business-lines", "", "逗号分隔的业务线（必填）")
	kernelURL := flags.String("kernel-url", "", "内核 HTTP 地址；为空时不启用编排")
	tokenEnv := flags.String("kernel-token-env", "", "服务令牌所在的环境变量名")
	consoleURL := flags.String("console-url", "", "内核控制台地址")
	mcpDir := flags.String("mcp-dir", "", "写入 MCP 登记的目录")
	storeKind := flags.String("store", "memory", "Persona 存储：memory | pg")
	personaDSN := flags.String("persona-dsn", os.Getenv("YANSHI_PERSONA_PG_DSN"), "Persona 独立数据库 DSN（YANSHI_PERSONA_PG_DSN；pg 必填）")
	appDSN := flags.String("app-dsn", os.Getenv("YANSHI_XINLI_PG_DSN"), "应用独立数据库 DSN（YANSHI_XINLI_PG_DSN；pg 编排必填）")
	exitOnEOF := flags.Bool("exit-on-stdin-eof", false, "stdin 关闭时退出")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("xinli dev: unexpected arguments")
	}
	var lines []string
	seen := map[string]bool{}
	// 业务线会成为文件名的一部分，拒绝路径分隔符和空段，避免覆盖目录外的文件。
	validLine := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	for _, raw := range strings.Split(*businessLines, ",") {
		bl := strings.TrimSpace(raw)
		if !validLine.MatchString(bl) {
			return errors.New("xinli dev: business-lines must contain nonempty IDs using letters, digits, _ or -")
		}
		if !seen[bl] {
			lines = append(lines, bl)
			seen[bl] = true
		}
	}
	for _, raw := range []string{*kernelURL, *consoleURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("xinli dev: invalid HTTP URL")
		}
	}
	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
		if token == "" {
			return errors.New("xinli dev: kernel token environment variable is empty")
		}
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	books, closeStore, err := openStore(ctx, *storeKind, *personaDSN)
	if err != nil {
		return err
	}
	defer closeStore()
	// 停机信号先关闭 HTTP 并排空归集，超时才取消归集 context。
	life, cancelLife := context.WithCancel(context.Background())
	defer cancelLife()
	var orch *orchestrator.Orchestrator
	opts := mcpserver.Options{}
	fixtureOpts := fixture.Options{}
	uiOpts := devui.Options{Store: books}
	if *kernelURL != "" {
		tasks, closeTasks, err := openTasks(life, *storeKind, *appDSN, *personaDSN)
		if err != nil {
			return err
		}
		defer closeTasks()
		client := &kernel.Client{BaseURL: *kernelURL, Token: token}
		orch = orchestrator.New(life, books, client, orchestrator.Options{Logger: logger, Tasks: tasks})
		fixtureOpts.Notifier = orch
		uiOpts.DaySessions, uiOpts.Kernel, uiOpts.Notifier = orch, client, orch
		opts.Notifier = orch
		defer func() {
			cancelLife()
			_ = orch.Shutdown(context.Background())
		}()
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.Handler(books, opts))
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
	mux.Handle("/", devui.Handler(devui.Config{BusinessLines: lines, ConsoleURL: *consoleURL}, uiOpts))
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return errors.New("xinli dev: listen failed")
	}
	defer listener.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return errors.New("xinli dev: invalid listener address")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base := "http://" + net.JoinHostPort(host, port)
	announcement := ready{MCPURL: base + "/mcp", FixtureURL: base, PageURL: base + "/", Registrations: registrations(lines, base+"/mcp")}
	if *mcpDir != "" {
		if err := os.MkdirAll(*mcpDir, 0755); err != nil {
			return errors.New("xinli dev: cannot create mcp-dir")
		}
		for _, r := range announcement.Registrations {
			// JSON 是 YAML 的子集；与就绪行使用同一序列化契约，避免两份标签漂移。
			data, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				return errors.New("xinli dev: cannot encode registration")
			}
			if err := os.WriteFile(filepath.Join(*mcpDir, "xinli-"+r.BusinessLine+".yaml"), append(data, '\n'), 0644); err != nil {
				return errors.New("xinli dev: cannot write registration")
			}
		}
	}
	if orch != nil {
		orch.StartDeletions()
		orch.StartTasks(nil)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	// stdout 是父进程的机器协议；就绪行之后只向 stderr 写日志。
	if err := json.NewEncoder(stdout).Encode(announcement); err != nil {
		_ = server.Close()
		return errors.New("xinli dev: cannot write readiness")
	}
	eof := make(chan struct{})
	if *exitOnEOF {
		go func() { _, _ = io.Copy(io.Discard, stdin); close(eof) }()
	}
	var result error
	select {
	case <-ctx.Done():
	case <-eof:
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.New("xinli dev: HTTP server failed")
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		result = errors.New("xinli dev: HTTP shutdown timed out")
	}
	if orch != nil {
		if err := orch.Shutdown(shutdown); err != nil {
			cancelLife()
			result = errors.New("xinli dev: orchestration shutdown timed out")
		}
	}
	return result
}

// 开发与夹具共用入口；迁移完成后才宣布就绪，DSN 不从内核配置回退。
func openStore(ctx context.Context, kind, dsn string) (store.Store, func(), error) {
	switch kind {
	case "memory":
		return memstore.New(), func() {}, nil
	case "pg":
		if strings.TrimSpace(dsn) == "" {
			return nil, nil, errors.New("xinli dev: pg requires -persona-dsn or YANSHI_PERSONA_PG_DSN (a separate database)")
		}
		startup, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		pool, err := pg.Open(startup, dsn, "")
		if err != nil {
			// 驱动错误可能包含连接信息，不能把 DSN 写入启动日志。
			return nil, nil, errors.New("xinli dev: cannot open Persona database")
		}
		if err := pg.Migrate(startup, pool); err != nil {
			pool.Close()
			return nil, nil, errors.New("xinli dev: Persona migration failed")
		}
		s := pgstore.New(pool)
		return s, s.Close, nil
	default:
		return nil, nil, errors.New("xinli dev: store must be memory or pg")
	}
}

// PG 编排不能确认到内存任务库；显式配置应用 DSN，避免重启后事实已确认而任务丢失。
func openTasks(ctx context.Context, kind, dsn, personaDSN string) (task.Store, func(), error) {
	if kind == "memory" {
		return taskmem.New(), func() {}, nil
	}
	if kind != "pg" {
		return nil, nil, errors.New("xinli dev: invalid task storage")
	}
	if dsn == "" {
		return nil, nil, errors.New("xinli dev: pg orchestration requires -app-dsn")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, errors.New("xinli dev: invalid application DSN")
	}
	// 开发入口要求库名也不同；只换 schema 不能构成三层数据库隔离。
	for _, other := range []string{personaDSN, os.Getenv("YANSHI_PG_DSN")} {
		if other == "" {
			continue
		}
		otherCfg, err := pgx.ParseConfig(other)
		if err != nil {
			return nil, nil, errors.New("xinli dev: invalid database configuration")
		}
		if cfg.Database == otherCfg.Database {
			return nil, nil, errors.New("xinli dev: application database must differ from Persona and kernel")
		}
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := appPG.Open(startup, dsn, "")
	if err != nil {
		return nil, nil, errors.New("xinli dev: cannot open application database")
	}
	if err := appPG.Migrate(startup, pool); err != nil {
		pool.Close()
		return nil, nil, errors.New("xinli dev: application migration failed")
	}
	return taskpg.New(pool), pool.Close, nil
}
