package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persona/mcpserver"
	personaPG "persona/pg/pgtest"
	appPG "xinli/pg/pgtest"

	"github.com/golang-jwt/jwt/v5"
)

func serviceTestToken(t *testing.T, line string) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"iss": line, "aud": "yanshi", "scope": "service", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	value, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func startService(t *testing.T, cfg config) ready {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	writer := readyWriter{make(chan []byte, 1)}
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, strings.NewReader(""), writer, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(12 * time.Second):
			t.Error("service shutdown timeout")
		}
	})
	select {
	case raw := <-writer.line:
		var info ready
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatal(err)
		}
		return info
	case err := <-done:
		// 仍让 Cleanup 消费，避免失败分支额外等待超时。
		done <- err
		t.Fatalf("service failed before readiness: %v", err)
	case <-time.After(40 * time.Second):
		t.Fatal("readiness timeout")
	}
	return ready{}
}

func status(t *testing.T, base, path string, want int) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s status=%d want=%d", path, resp.StatusCode, want)
	}
}

func TestServeRoleAssembly(t *testing.T) {
	for _, role := range []roles{{true, true, true}, {true, false, false}, {false, true, false}, {false, false, true}} {
		t.Run(strings.Join(role.names(), ","), func(t *testing.T) {
			// 用同一生产装配验证角色；CLI 的生产存储约束另由真实 PG 测试覆盖。
			cfg := config{mode: "serve", store: "memory", addr: "127.0.0.1:0", healthAddr: "127.0.0.1:0", lines: []string{"bl"}, roles: role, kernelURL: "http://127.0.0.1:1", autoSchedule: true}
			cfg.credentials = []mcpserver.Credential{{BusinessLine: "bl", Token: "test", ExpiresAt: time.Now().Add(time.Hour)}}
			info := startService(t, cfg)
			status(t, info.HealthURL, "/healthz", 200)
			status(t, info.HealthURL, "/readyz", 200)
			status(t, info.HealthURL, "/mcp", 404)
			if info.FixtureURL != "" || info.PageURL != "" {
				t.Fatal("production exposed development URL")
			}
			if role.persona || role.bff {
				base := info.APIURL
				for _, path := range []string{"/", "/app.js", "/ui.js", "/style.css", "/config.json", "/v1/fixture/bl/u/state", "/v1/dev/bl/u/captures"} {
					status(t, base, path, 404)
				}
				if role.persona {
					status(t, base, "/mcp", 401)
				} else {
					status(t, base, "/mcp", 404)
				}
			}
			if !role.persona && (info.MCPURL != "" || len(info.Registrations) != 0) {
				t.Fatal("non-persona role exposed MCP")
			}
		})
	}
}

func productionArgs(t *testing.T, personaDSN, appDSN, kernelURL string) []string {
	t.Helper()
	t.Setenv("TEST_MCP_SECRET", "production-test-secret")
	refs := `[{"business_line":"bl","token_env":"TEST_MCP_SECRET","expires_at":"2099-01-01T00:00:00Z"}]`
	t.Setenv("TEST_MCP_CONFIG", refs)
	t.Setenv("TEST_KERNEL_TOKEN", serviceTestToken(t, "bl"))
	return []string{"serve", "-business-lines", "bl", "-persona-dsn", personaDSN, "-app-dsn", appDSN, "-kernel-url", kernelURL, "-kernel-token-env", "TEST_KERNEL_TOKEN", "-mcp-credentials-env", "TEST_MCP_CONFIG", "-addr", "127.0.0.1:0", "-health-addr", "127.0.0.1:0"}
}

func TestProductionConfiguration(t *testing.T) {
	t.Setenv("YANSHI_PG_DSN", "")
	args := productionArgs(t, "postgres://localhost/persona", "postgres://localhost/xinli", "http://localhost:8080")
	cfg, err := parseConfig(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.roles.names()) != 3 || cfg.store != "pg" || cfg.tokens == nil || cfg.headers["bl"]["Authorization"] != "Bearer ${TEST_MCP_SECRET}" {
		t.Fatal("production defaults")
	}
	for _, extra := range [][]string{{"-insecure-dev-mcp"}, {"-store", "memory"}, {"-roles", "unknown"}, {"-roles", ""}, {"-kernel-url", ""}, {"-app-dsn", "postgres://localhost/persona"}, {"-business-lines", "bl,other"}} {
		if _, err := parseConfig(append(append([]string{}, args...), extra...), io.Discard); err == nil {
			t.Fatalf("accepted invalid flags: %v", extra)
		}
	}
	t.Setenv("XINLI_BUSINESS_LINES", "bl")
	t.Setenv("XINLI_ROLES", "persona")
	t.Setenv("YANSHI_PERSONA_PG_DSN", "postgres://localhost/persona")
	t.Setenv("XINLI_MCP_CREDENTIALS_ENV", "TEST_MCP_CONFIG")
	if cfg, err := parseConfig([]string{"serve"}, io.Discard); err != nil || cfg.roles.orchestrator || !cfg.roles.persona {
		t.Fatalf("environment configuration: %v", err)
	}
	if _, err := parseConfig([]string{"dev", "-addr", ":8081"}, io.Discard); err == nil {
		t.Fatal("public fixture accepted")
	}
	if _, err := parseConfig([]string{"migrate", "-persona-dsn", "postgres://localhost/persona", "-app-dsn", "postgres://localhost/persona"}, io.Discard); err == nil {
		t.Fatal("same database migration accepted")
	}
}

func TestServePostgresCombinedAndSplit(t *testing.T) {
	personaPool, personaDSN := personaPG.NewWithDSN(t)
	_, appDSN := appPG.NewWithDSN(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer backend.Close()
	args := productionArgs(t, personaDSN, appDSN, backend.URL)
	if err := run(context.Background(), []string{"migrate", "-persona-dsn", personaDSN, "-app-dsn", appDSN}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	var announcements []ready
	for _, role := range []string{"persona,orchestrator,bff", "persona", "orchestrator", "bff"} {
		cfg, err := parseConfig(append(append([]string{}, args...), "-roles", role), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		// 后台角色无需占用 addr：预先占住它，只有 health-addr 会被监听。
		if role == "orchestrator" {
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer occupied.Close()
			cfg.addr = occupied.Addr().String()
		}
		processArgs := append(append([]string{}, args...), "-roles", role, "-addr", cfg.addr)
		info := startProductionProcess(t, processArgs)
		announcements = append(announcements, info)
		status(t, info.HealthURL, "/readyz", 200)
	}
	// 模拟迁移账本缺失：就绪与再次启动都拒绝，serve 不偷偷执行迁移。
	if _, err := personaPool.Exec(context.Background(), `DELETE FROM persona_schema_migrations WHERE version=(SELECT max(version) FROM persona_schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	for _, info := range announcements {
		status(t, info.HealthURL, "/readyz", 503)
	}
	cfg, err := parseConfig(append(args, "-roles", "persona"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(context.Background(), cfg, strings.NewReader(""), io.Discard, io.Discard); err == nil {
		t.Fatal("serve accepted pending migrations")
	}
}

func TestCredentialConfigurationRotation(t *testing.T) {
	t.Setenv("TEST_MCP_OLD", "old-secret")
	t.Setenv("TEST_MCP_NEW", "new-secret")
	data := `[{"business_line":"bl","token_env":"TEST_MCP_OLD","expires_at":"2099-01-01T00:00:00Z"},{"business_line":"bl","token_env":"TEST_MCP_NEW","expires_at":"2099-01-01T00:00:00Z"}]`
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config{mode: "serve", lines: []string{"bl"}}
	if err := cfg.configureMCP(path, ""); err != nil {
		t.Fatal(err)
	}
	if len(cfg.credentials) != 2 || cfg.headers["bl"]["Authorization"] != "Bearer ${TEST_MCP_NEW}" {
		t.Fatal("rotation registration")
	}
	raw, _ := json.Marshal(registration{Headers: cfg.headers["bl"]})
	if strings.Contains(string(raw), "secret") {
		t.Fatal("production registration leaked secret")
	}
}

// 同一测试二进制以真实 serve 子命令启动，证明角色不依赖进程内通知或共享全局对象。
func startProductionProcess(t *testing.T, args []string) ready {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdin, input := io.Pipe()
	output, stdout := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runDevProcess(ctx, append(args, "-exit-on-stdin-eof"), stdin, stdout, io.Discard)
		_ = stdout.Close()
	}()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(12 * time.Second):
			cancel()
			t.Error("process shutdown timeout")
		}
		cancel()
		_ = stdin.Close()
		_ = output.Close()
	})
	type received struct {
		info ready
		err  error
	}
	result := make(chan received, 1)
	go func() {
		var info ready
		err := json.NewDecoder(output).Decode(&info)
		result <- received{info, err}
		_, _ = io.Copy(io.Discard, output)
	}()
	select {
	case r := <-result:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.info
	case <-time.After(75 * time.Second):
		t.Fatal("process readiness timeout")
	}
	return ready{}
}
