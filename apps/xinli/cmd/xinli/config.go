package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"persona/mcpserver"
	"persona/pg"
	"xinli/account"
	"xinli/kernel"
	appPG "xinli/pg"

	"github.com/jackc/pgx/v5"
)

type roles struct {
	persona, orchestrator, bff bool
}

func (r roles) names() []string {
	var out []string
	if r.persona {
		out = append(out, "persona")
	}
	if r.orchestrator {
		out = append(out, "orchestrator")
	}
	if r.bff {
		out = append(out, "bff")
	}
	return out
}

type config struct {
	mode             string
	addr             string
	healthAddr       string
	store            string
	personaDSN       string
	appDSN           string
	personaURL       string
	kernelURL        string
	consoleURL       string
	mcpDir           string
	lines            []string
	roles            roles
	autoSchedule     bool
	exitOnEOF        bool
	insecureMCP      bool
	credentials      []mcpserver.Credential
	headers          map[string]map[string]string
	tokens           kernel.TokenSource
	identityProvider account.IdentityProvider
}

func envDefault(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	cfg := config{}
	if len(args) == 0 || (args[0] != "dev" && args[0] != "serve" && args[0] != "migrate") {
		return cfg, errors.New("usage: xinli serve | migrate | dev")
	}
	cfg.mode = args[0]
	flags := flag.NewFlagSet("xinli "+cfg.mode, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.personaDSN, "persona-dsn", os.Getenv("YANSHI_PERSONA_PG_DSN"), "Persona 独立数据库 DSN")
	flags.StringVar(&cfg.appDSN, "app-dsn", os.Getenv("YANSHI_XINLI_PG_DSN"), "应用独立数据库 DSN")
	if cfg.mode == "migrate" {
		if err := flags.Parse(args[1:]); err != nil {
			return cfg, err
		}
		if flags.NArg() != 0 || cfg.personaDSN == "" || cfg.appDSN == "" {
			return cfg, errors.New("xinli migrate: both database DSNs required")
		}
		return cfg, validateDatabases(cfg.personaDSN, cfg.appDSN)
	}
	addr, kind := ":8081", "pg"
	if cfg.mode == "dev" {
		addr, kind = "127.0.0.1:0", "memory"
	}
	flags.StringVar(&cfg.addr, "addr", envDefault("XINLI_ADDR", addr), "Persona/BFF 监听地址")
	flags.StringVar(&cfg.healthAddr, "health-addr", envDefault("XINLI_HEALTH_ADDR", ":9091"), "serve 健康探针监听地址")
	flags.StringVar(&cfg.store, "store", envDefault("XINLI_STORE", kind), "dev: memory | pg；serve: pg")
	flags.StringVar(&cfg.personaURL, "persona-url", os.Getenv("XINLI_PERSONA_URL"), "BFF 的 Persona 查询 HTTP 地址；为空时使用进程内 API")
	flags.StringVar(&cfg.kernelURL, "kernel-url", os.Getenv("XINLI_KERNEL_URL"), "内核 HTTP 地址")
	flags.StringVar(&cfg.consoleURL, "console-url", os.Getenv("XINLI_CONSOLE_URL"), "dev 内核控制台地址")
	flags.StringVar(&cfg.mcpDir, "mcp-dir", os.Getenv("XINLI_MCP_DIR"), "MCP 登记输出目录")
	lines := flags.String("business-lines", os.Getenv("XINLI_BUSINESS_LINES"), "逗号分隔的业务线")
	roleList := flags.String("roles", envDefault("XINLI_ROLES", "persona,orchestrator,bff"), "serve 角色：persona,orchestrator,bff")
	mcpFile := flags.String("mcp-credentials-file", os.Getenv("XINLI_MCP_CREDENTIALS_FILE"), "MCP 凭据引用 JSON 文件")
	mcpEnv := flags.String("mcp-credentials-env", os.Getenv("XINLI_MCP_CREDENTIALS_ENV"), "MCP 凭据引用 JSON 所在环境变量名")
	tokenFile := flags.String("kernel-tokens-file", os.Getenv("XINLI_KERNEL_TOKENS_FILE"), "按业务线的内核凭据来源 JSON 文件")
	tokenEnv := flags.String("kernel-tokens-env", os.Getenv("XINLI_KERNEL_TOKENS_ENV"), "按业务线的内核凭据来源 JSON 所在环境变量名")
	testFile := flags.String("test-identities-file", os.Getenv("XINLI_TEST_IDENTITIES_FILE"), "测试身份配置引用 JSON 文件")
	testEnv := flags.String("test-identities-env", os.Getenv("XINLI_TEST_IDENTITIES_ENV"), "测试身份配置引用 JSON 所在环境变量名")
	legacyTokenEnv := flags.String("kernel-token-env", os.Getenv("XINLI_KERNEL_TOKEN_ENV"), "单业务线静态服务令牌所在环境变量名")
	audience := flags.String("kernel-audience", envDefault("XINLI_KERNEL_AUDIENCE", "yanshi"), "内核 JWT audience")
	ttl := flags.String("kernel-token-ttl", envDefault("XINLI_KERNEL_TOKEN_TTL", "15m"), "签发令牌有效期，至多 1h 且不超过内核 max_ttl")
	refresh := flags.String("kernel-token-refresh-before", envDefault("XINLI_KERNEL_TOKEN_REFRESH_BEFORE", "1m"), "过期前刷新窗口")
	// 环境布尔值也交给 flag 解析，错误不应悄悄回退成默认配置。
	boolFlag := func(target *bool, name, env string, fallback bool) error {
		value, err := strconv.ParseBool(envDefault(env, strconv.FormatBool(fallback)))
		if err != nil {
			return errors.New("xinli: invalid boolean environment configuration")
		}
		flags.BoolVar(target, name, value, name)
		return nil
	}
	for _, err := range []error{
		boolFlag(&cfg.autoSchedule, "auto-schedule", "XINLI_AUTO_SCHEDULE", true),
		boolFlag(&cfg.exitOnEOF, "exit-on-stdin-eof", "XINLI_EXIT_ON_STDIN_EOF", false),
		boolFlag(&cfg.insecureMCP, "insecure-dev-mcp", "XINLI_INSECURE_DEV_MCP", false),
	} {
		if err != nil {
			return cfg, err
		}
	}
	if err := flags.Parse(args[1:]); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("xinli: unexpected arguments")
	}
	validLine := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	seen := map[string]bool{}
	for _, raw := range strings.Split(*lines, ",") {
		line := strings.TrimSpace(raw)
		if !validLine.MatchString(line) {
			return cfg, errors.New("xinli: invalid business-lines")
		}
		if !seen[line] {
			cfg.lines = append(cfg.lines, line)
			seen[line] = true
		}
	}
	if *testFile != "" || *testEnv != "" {
		var refs []struct {
			BusinessLine string `json:"business_line"`
			Subject      string `json:"subject"`
			CodeEnv      string `json:"code_env"`
		}
		if err := readConfigJSON(*testFile, *testEnv, &refs); err != nil {
			return cfg, err
		}
		provider := account.TestProvider{}
		validEnv := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
		proofs := map[string]bool{}
		for _, ref := range refs {
			if !seen[ref.BusinessLine] || ref.Subject == "" || !validEnv.MatchString(ref.CodeEnv) {
				return cfg, errors.New("xinli: invalid test identity configuration")
			}
			code := os.Getenv(ref.CodeEnv)
			key := ref.BusinessLine + ":" + account.Hash(code)
			if len(code) < 16 || proofs[key] {
				return cfg, errors.New("xinli: test identity secret missing, short or duplicated")
			}
			proofs[key] = true
			provider.Identities = append(provider.Identities, account.TestIdentity{BusinessLine: ref.BusinessLine, Subject: ref.Subject, Code: code})
		}
		cfg.identityProvider = provider
	}
	if cfg.mode == "dev" {
		cfg.roles = roles{persona: true, orchestrator: cfg.kernelURL != ""}
		host, _, err := net.SplitHostPort(cfg.addr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return cfg, errors.New("xinli dev: fixture requires a loopback address")
		}
	} else {
		for _, role := range strings.Split(*roleList, ",") {
			switch strings.TrimSpace(role) {
			case "persona":
				cfg.roles.persona = true
			case "orchestrator":
				cfg.roles.orchestrator = true
			case "bff":
				cfg.roles.bff = true
			default:
				return cfg, errors.New("xinli: invalid roles")
			}
		}
		if cfg.insecureMCP || cfg.store != "pg" {
			return cfg, errors.New("xinli serve: pg and authenticated MCP required")
		}
	}
	if cfg.store != "memory" && cfg.store != "pg" {
		return cfg, errors.New("xinli: invalid store")
	}
	if cfg.store == "pg" {
		if cfg.personaDSN == "" || (cfg.roles.orchestrator || cfg.roles.bff || cfg.identityProvider != nil) && cfg.appDSN == "" {
			return cfg, errors.New("xinli: required database DSN missing")
		}
		if err := validateDatabases(cfg.personaDSN, cfg.appDSN); err != nil {
			return cfg, err
		}
	}
	for _, raw := range []string{cfg.kernelURL, cfg.consoleURL, cfg.personaURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return cfg, errors.New("xinli: invalid HTTP URL")
		}
	}
	if cfg.roles.orchestrator || cfg.roles.bff {
		if cfg.kernelURL == "" {
			return cfg, errors.New("xinli: orchestrator and bff require kernel-url")
		}
		var locations map[string]kernel.TokenLocation
		configured := *tokenFile != "" || *tokenEnv != ""
		if configured {
			if err := readConfigJSON(*tokenFile, *tokenEnv, &locations); err != nil {
				return cfg, err
			}
		}
		if *legacyTokenEnv != "" {
			if configured || len(cfg.lines) != 1 {
				return cfg, errors.New("xinli: kernel-token-env requires exactly one business line and no token map")
			}
			locations = map[string]kernel.TokenLocation{cfg.lines[0]: {TokenEnv: *legacyTokenEnv}}
			configured = true
		}
		if !configured && cfg.mode == "serve" {
			return cfg, errors.New("xinli: kernel token source required")
		}
		if configured {
			duration, e1 := time.ParseDuration(*ttl)
			before, e2 := time.ParseDuration(*refresh)
			if e1 != nil || e2 != nil {
				return cfg, errors.New("xinli: invalid token duration")
			}
			var err error
			cfg.tokens, err = kernel.NewTokenSource(locations, kernel.TokenOptions{Audience: *audience, TTL: duration, RefreshBefore: before})
			if err != nil {
				return cfg, err
			}
			for _, line := range cfg.lines {
				if _, err := cfg.tokens.Token(context.Background(), line); err != nil {
					return cfg, err
				}
			}
		}
	}
	if !cfg.roles.persona && cfg.mcpDir != "" {
		return cfg, errors.New("xinli: mcp-dir requires persona role")
	}
	if cfg.roles.persona || cfg.roles.bff && cfg.personaURL != "" {
		if err := cfg.configureMCP(*mcpFile, *mcpEnv); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func readConfigJSON(file, env string, out any) error {
	if (file == "") == (env == "") {
		return errors.New("xinli: select one configuration file or environment variable")
	}
	var data []byte
	if file != "" {
		var err error
		data, err = os.ReadFile(file)
		if err != nil {
			return errors.New("xinli: cannot read credential configuration")
		}
	} else {
		data = []byte(os.Getenv(env))
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("xinli: invalid credential configuration")
	}
	return nil
}

func (cfg *config) configureMCP(file, env string) error {
	cfg.headers = map[string]map[string]string{}
	if cfg.insecureMCP {
		if file != "" || env != "" {
			return errors.New("xinli: insecure MCP conflicts with credentials")
		}
		return nil
	}
	if file == "" && env == "" && cfg.mode == "dev" {
		for _, line := range cfg.lines {
			var secret [32]byte
			if _, err := rand.Read(secret[:]); err != nil {
				return errors.New("xinli: cannot create development credential")
			}
			token := hex.EncodeToString(secret[:])
			cfg.credentials = append(cfg.credentials, mcpserver.Credential{BusinessLine: line, Token: token, ExpiresAt: time.Now().Add(24 * time.Hour)})
			cfg.headers[line] = map[string]string{"Authorization": "Bearer " + token}
		}
		return nil
	}
	var refs []struct {
		BusinessLine string    `json:"business_line"`
		TokenEnv     string    `json:"token_env"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
	if err := readConfigJSON(file, env, &refs); err != nil {
		return err
	}
	validEnv := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	allowed := map[string]bool{}
	for _, line := range cfg.lines {
		allowed[line] = true
	}
	for _, ref := range refs {
		if !allowed[ref.BusinessLine] || !validEnv.MatchString(ref.TokenEnv) {
			return errors.New("xinli: invalid MCP credential reference")
		}
		token := os.Getenv(ref.TokenEnv)
		cfg.credentials = append(cfg.credentials, mcpserver.Credential{BusinessLine: ref.BusinessLine, Token: token, ExpiresAt: ref.ExpiresAt})
		if time.Now().Before(ref.ExpiresAt) {
			cfg.headers[ref.BusinessLine] = map[string]string{"Authorization": "Bearer ${" + ref.TokenEnv + "}"}
		}
	}
	if err := mcpserver.ValidateCredentials(cfg.credentials); err != nil {
		return err
	}
	for _, line := range cfg.lines {
		if cfg.headers[line] == nil {
			return errors.New("xinli: active MCP credential required for every business line")
		}
	}
	return nil
}

// 三层库名必须不同；只换 schema 不构成故障域隔离。内核 DSN 仅用于拒绝误配，不连接或迁移它。
func validateDatabases(personaDSN, appDSN string) error {
	seen := map[string]bool{}
	for _, dsn := range []string{personaDSN, appDSN, os.Getenv("YANSHI_PG_DSN")} {
		if dsn == "" {
			continue
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil || cfg.Database == "" || seen[cfg.Database] {
			return errors.New("xinli: databases must be valid and distinct")
		}
		seen[cfg.Database] = true
	}
	return nil
}

func migrateDatabases(ctx context.Context, cfg config) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	personaPool, err := pg.Open(ctx, cfg.personaDSN, "")
	if err != nil {
		return errors.New("xinli: cannot open Persona database")
	}
	defer personaPool.Close()
	appPool, err := appPG.Open(ctx, cfg.appDSN, "")
	if err != nil {
		return errors.New("xinli: cannot open application database")
	}
	defer appPool.Close()
	if pg.Migrate(ctx, personaPool) != nil {
		return errors.New("xinli: Persona migration failed")
	}
	if appPG.Migrate(ctx, appPool) != nil {
		return errors.New("xinli: application migration failed")
	}
	return nil
}
