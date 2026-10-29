// Package day 以真实三库与独立 xinli serve 进程验收 Q122；内核不依赖业务模块。
package day

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/auth"
	"yanshi/internal/capability"
	"yanshi/internal/card"
	"yanshi/internal/clock"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/janitor"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
	"yanshi/internal/model/openaicompat"
	"yanshi/internal/model/script"
	"yanshi/internal/node"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/runtime"
	"yanshi/internal/shard"
	"yanshi/internal/workqueue"
)

const businessLine = "daytest"
const businessDay = "2035-01-08"

type environment struct {
	t                                                      *testing.T
	ctx                                                    context.Context
	root, bin                                              string
	control                                                *control
	core                                                   *app.Core
	janitor                                                *janitor.Janitor
	kernel                                                 *httptest.Server
	kernelPool, personaPool, appPool                       *pgxpool.Pool
	personaDSN, appDSN                                     string
	dbProxy                                                *tcpProxy
	persona, bff                                           *frontProxy
	processes                                              map[string]*process
	processHistory                                         []*process
	diagnosticOnce                                         sync.Once
	kernelHTTP                                             *httpTrace
	script                                                 *scriptTrace
	environ                                                []string
	token, refreshToken, endUser, serviceToken, senderPath string
	accessExpires                                          time.Time
	ark                                                    bool
	locator                                                shard.Locator
	shardProxies                                           map[int]*tcpProxy
}

type process struct {
	cmd                        *exec.Cmd
	done                       chan struct{}
	once                       sync.Once
	api                        string
	health                     string
	role                       string
	startedAt, startedBusiness time.Time
	startedOffset              int64
	stderr                     *lineTail
}

func (p *process) kill() {
	p.once.Do(func() { _ = p.cmd.Process.Kill(); <-p.done })
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func randomID(t *testing.T) string {
	var data [24]byte
	_, err := rand.Read(data[:])
	must(t, err)
	return hex.EncodeToString(data[:])
}

func newEnvironment(t *testing.T, ark bool) *environment { return newEnvironmentShards(t, ark, false) }

func newEnvironmentShards(t *testing.T, ark, multiple bool) *environment {
	t.Helper()
	bin := os.Getenv("YANSHI_DAY_BIN")
	if bin == "" {
		t.Skip("set YANSHI_DAY_BIN to the daytest xinli binary; use make day")
	}
	if os.Getenv("YANSHI_TEST_PG") == "" {
		t.Fatal("day requires YANSHI_TEST_PG")
	}
	root, err := filepath.Abs("../..")
	must(t, err)
	bin, err = filepath.Abs(bin)
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	e := &environment{t: t, ctx: ctx, root: root, bin: bin, ark: ark, processes: map[string]*process{}, senderPath: filepath.Join(t.TempDir(), "sender.json")}
	e.control = newControl(t)
	e.persona, e.bff = newFront(t), newFront(t)
	pool, notifier := pgtest.Fresh(t)
	e.kernelPool = pool
	e.personaPool, e.personaDSN = freshBusinessDB(t, "yanshi_day_persona")
	e.appPool, e.appDSN = freshBusinessDB(t, "yanshi_day_app")
	databases := map[string]bool{}
	for _, db := range []*pgxpool.Pool{e.kernelPool, e.personaPool, e.appPool} {
		var name string
		must(t, db.QueryRow(ctx, `SELECT current_database()`).Scan(&name))
		if databases[name] {
			t.Fatal("day requires three independent databases")
		}
		databases[name] = true
	}
	e.dbProxy, e.personaDSN = newTCPProxy(t, e.personaDSN)
	t.Cleanup(e.diagnoseIfFailed)
	key, err := auth.GenerateDevKey(businessLine, "day")
	must(t, err)
	public, err := key.Public()
	must(t, err)
	verifier, err := auth.NewJWT("yanshi", clock.Real{}, public)
	must(t, err)
	e.serviceToken, err = key.Sign("yanshi", "", time.Hour, time.Now())
	must(t, err)
	mcpToken := randomID(t)
	e.environ = append(os.Environ(), "YANSHI_PERSONA_PG_DSN="+e.personaDSN, "YANSHI_XINLI_PG_DSN="+e.appDSN,
		"YANSHI_PG_DSN="+os.Getenv("YANSHI_TEST_PG"), "DAY_KERNEL_TOKEN="+e.serviceToken,
		"DAY_MCP_TOKEN="+mcpToken, "XINLI_DAY_CONTROL_TOKEN="+e.control.token,
		`DAY_MCP_CONFIG=[{"business_line":"daytest","token_env":"DAY_MCP_TOKEN","expires_at":"2099-01-01T00:00:00Z"}]`,
		`DAY_IDENTITIES=[{"business_line":"daytest","subject":"day-user","code_env":"DAY_IDENTITY_CODE"}]`, "DAY_IDENTITY_CODE="+randomID(t))
	migration := exec.CommandContext(ctx, bin, "migrate")
	migration.Env, migration.Stdout, migration.Stderr = e.environ, io.Discard, io.Discard
	if migration.Run() != nil {
		t.Fatal("xinli migrations failed")
	}

	var defs []*agentdef.Def
	for _, name := range []string{"xinli-day", "xinli-project", "xinli-week", "xinli-assistant"} {
		raw, err := os.ReadFile(filepath.Join(root, "agents", name+".yaml"))
		must(t, err)
		var def agentdef.Def
		must(t, yaml.Unmarshal(raw, &def))
		if !ark {
			def.Model = "day/" + name
		}
		if name == "xinli-assistant" && !ark {
			def.Capabilities = append(def.Capabilities, "approval_probe")
		}
		defs = append(defs, &def)
	}
	if multiple {
		defs = append(defs, &agentdef.Def{Name: "shard-parent", Version: "1", Model: "shardtest/any", Capabilities: []string{"agent:shard-helper"}},
			&agentdef.Def{Name: "shard-helper", Version: "1", Model: "shardtest/any", Description: "分片委托测试"})
	}
	agents, err := agentdef.NewRegistry(defs...)
	must(t, err)
	gateway := model.NewGateway()
	e.script = &scriptTrace{}
	gateway.Register("day", dayModel{trace: e.script})
	if multiple {
		gateway.Register("shardtest", script.Provider{})
	}
	if ark {
		if os.Getenv("ARK_API_KEY") == "" {
			t.Fatal("ARK_API_KEY required")
		}
		base := os.Getenv("ARK_BASE_URL")
		if base == "" {
			base = openaicompat.ArkBaseURL
		}
		gateway.Register("ark", &openaicompat.Provider{BaseURL: base, APIKey: os.Getenv("ARK_API_KEY")})
	}
	registration := mcpcap.Server{Name: "xinli", BusinessLine: businessLine, URL: e.persona.server.URL + "/mcp", Headers: map[string]string{"Authorization": "Bearer " + mcpToken}, Timeout: 15 * time.Second, CiteCaptures: true, AttestsSpeakers: true}
	registration.Tools.NoApproval = []string{"file_changes", "file_day", "set_status", "update_commitments", "propose_merge", "file_week"}
	stores := app.Postgres(pool, notifier, e.control)
	if multiple {
		e.locator, err = shard.ParseDeployment([]byte(`{"table":{"shards":[0,1],"ranges":[{"first":0,"last":511,"shard":0},{"first":512,"last":1023,"shard":1}]},"compatibility":{"version":"day-dual"}}`))
		must(t, err)
		firstProxy, firstDSN := newTCPProxy(t, pgtest.Database(t))
		secondProxy, secondDSN := newTCPProxy(t, pgtest.Database(t))
		e.shardProxies = map[int]*tcpProxy{0: firstProxy, 1: secondProxy}
		first, firstNotifier := pgtest.FreshAt(t, firstDSN)
		second, secondNotifier := pgtest.FreshAt(t, secondDSN)
		e.kernelPool = first
		stores, err = app.Sharded(ctx, e.locator, map[int]app.Stores{
			0: app.PostgresShard(first, firstNotifier, e.control, e.locator, 0),
			1: app.PostgresShard(second, secondNotifier, e.control, e.locator, 1),
		}, app.ShardOptions{Now: e.control.Now})
		must(t, err)
	}
	connector := &mcpcap.Connector{Servers: []mcpcap.Server{registration}, Log: stores.Log, CacheTTL: 200 * time.Millisecond}
	local := []capability.Capability{capability.ClockNow(e.control), card.New(stores.ArtifactMeta), capability.Func{
		S: capability.Spec{Name: "approval_probe", Risk: v1.Risk_RISK_HIGH, Idempotent: true, InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		Fn: func(context.Context, capability.Invocation) ([]*v1.ContentBlock, error) {
			return model.TextBlocks("已确认"), nil
		},
	}}
	e.core = app.NewCore(stores, app.Options{Clock: e.control, IDs: ids.Sequential, Agents: agents, Model: gateway, MCP: connector,
		Blobs: artifact.NewMemBlobs(), NodeAuth: node.TokenAuth{Verifier: verifier}, Local: local, LeaseTTL: 5 * time.Second, DefaultTimeout: time.Minute})
	connector.Artifacts = e.core.Artifacts
	e.kernelHTTP = &httpTrace{}
	e.kernel = httptest.NewServer(e.kernelHTTP.handler((&httpapi.Server{Auth: verifier, Service: e.core.Service, Outbox: stores.Outbox, OutboxWait: 100 * time.Millisecond,
		OutboxRecheck: 10 * time.Millisecond, Artifacts: e.core.Artifacts, Memory: e.core.Memory}).Handler()))
	var workers sync.WaitGroup
	for i := range 2 {
		cfg := e.core.WorkerConfig(fmt.Sprintf("day-%d", i), workqueue.Pool{}, nil)
		cfg.IdleWait = 10 * time.Millisecond
		workers.Add(1)
		go func() { defer workers.Done(); runtime.New(cfg).Run(ctx) }()
	}
	e.janitor = e.core.Janitor("day-janitor", nil, nil, 0)
	// 不启动后台 Sweep；拨钟后显式 Sweep，避免 72h 测试的保留观察点受调度影响。
	cleanup := e.core.Janitor("day-cleanup", nil, nil, 0)
	cleanup.IdleWait = 10 * time.Millisecond
	workers.Add(1)
	go func() {
		defer workers.Done()
		for ctx.Err() == nil {
			_, _ = cleanup.Step(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { cancel(); workers.Wait(); connector.Close(); e.kernel.Close() })
	e.start("persona")
	e.start("bff")
	e.login()
	return e
}

func (e *environment) start(role string) {
	e.t.Helper()
	if old := e.processes[role]; old != nil {
		old.kill()
	}
	args := []string{"serve", "-roles", role, "-business-lines", businessLine, "-addr", "127.0.0.1:0", "-health-addr", "127.0.0.1:0",
		"-kernel-url", e.kernel.URL, "-kernel-token-env", "DAY_KERNEL_TOKEN", "-mcp-credentials-env", "DAY_MCP_CONFIG",
		"-test-identities-env", "DAY_IDENTITIES", "-persona-url", e.persona.server.URL,
		"-test-mode", "-test-control-url", e.control.server.URL}
	args = isolatedServeArgs(args)
	cmd := exec.CommandContext(e.ctx, e.bin, args...)
	p := &process{cmd: cmd, done: make(chan struct{}), role: role, stderr: &lineTail{},
		startedAt: time.Now().UTC(), startedBusiness: e.control.Now().UTC(), startedOffset: e.control.offset.Load()}
	cmd.Env, cmd.Stderr = e.environ, p.stderr
	stdout, err := cmd.StdoutPipe()
	must(e.t, err)
	must(e.t, cmd.Start())
	ready := make(chan []byte, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- append([]byte(nil), scanner.Bytes()...)
		} else {
			ready <- nil
		}
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
		close(p.done)
	}()
	e.processes[role] = p
	e.processHistory = append(e.processHistory, p)
	// 必须在 kill、取消父进程和关闭三库之前取现场；同一失败只输出一次。
	e.t.Cleanup(func() { e.diagnoseIfFailed(); p.kill() })
	select {
	case raw := <-ready:
		var result struct {
			API    string `json:"api_url"`
			Health string `json:"health_url"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &result) != nil || result.Health == "" {
			e.t.Fatal("xinli role failed before ready: " + role)
		}
		p.api, p.health = result.API, result.Health
		if role == "persona" {
			e.persona.set(e.t, p.api)
		}
		if role == "bff" {
			e.bff.set(e.t, p.api)
		}
		e.roleReady(p)
		e.checkClock(p)
		if (role == "persona" || role == "bff") && e.token != "" {
			if bff := e.processes["bff"]; bff != nil && bff.running() {
				e.awaitBFFRead("BFF reaches restarted Persona", "/v1/commitments?status=all", nil)
			}
		}
	case <-time.After(40 * time.Second):
		e.t.Fatal("role readiness timed out: " + role)
	}
}
func (e *environment) stop(role string) {
	if p := e.processes[role]; p != nil {
		p.kill()
	}
}

func request(ctx context.Context, address, method, token string, in any) (int, json.RawMessage) {
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 35 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw
}
func (e *environment) bffRequest(method, path string, in any, want int, out any) {
	e.t.Helper()
	if !strings.HasPrefix(path, "/v1/auth/") {
		e.ensureAuth()
	}
	code, raw := request(e.ctx, e.bff.server.URL+path, method, e.token, in)
	if code != want {
		e.t.Fatalf("%s %s: status %d want %d", method, path, code, want)
	}
	if out != nil {
		must(e.t, json.Unmarshal(raw, out))
	}
}
func (e *environment) login() {
	proof := ""
	for _, value := range e.environ {
		if v, ok := strings.CutPrefix(value, "DAY_IDENTITY_CODE="); ok {
			proof = v
		}
	}
	var out struct {
		Access  string                   `json:"access_token"`
		Refresh string                   `json:"refresh_token"`
		Expires time.Time                `json:"access_expires_at"`
		Owner   struct{ EndUser string } `json:"owner"`
	}
	e.bffRequest("POST", "/v1/auth/login", map[string]string{"business_line": businessLine, "proof": proof, "device_id": "phone", "platform": "test"}, 200, &out)
	e.token, e.refreshToken, e.accessExpires, e.endUser = out.Access, out.Refresh, out.Expires, out.Owner.EndUser
	if e.token == "" || e.endUser == "" {
		e.t.Fatal("login returned no identity")
	}
}

// 与普通 PG 测试共用实例，但每层独立 database，每次验收独占 schema。
func freshBusinessDB(t *testing.T, database string) (*pgxpool.Pool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := os.Getenv("YANSHI_TEST_PG")
	u, err := url.Parse(dsn)
	must(t, err)
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatal("day requires a PostgreSQL URL DSN")
	}
	admin, err := pgx.Connect(ctx, dsn)
	must(t, err)
	defer admin.Close(context.Background())
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, database)
	must(t, err)
	var exists bool
	must(t, admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, database).Scan(&exists))
	if !exists {
		_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
		must(t, err)
	}
	u.Path = "/" + database
	values := u.Query()
	values.Del("search_path")
	u.RawQuery = values.Encode()
	adminDSN := u.String()
	conn, err := pgx.Connect(ctx, adminDSN)
	must(t, err)
	defer conn.Close(context.Background())
	schema := "day_" + randomID(t)
	_, err = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			t.Error("cannot clean day schema")
			return
		}
		defer conn.Close(context.Background())
		if _, err = conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error("cannot drop day schema")
		}
	})
	values.Set("search_path", schema)
	u.RawQuery = values.Encode()
	pool, err := pgxpool.New(ctx, u.String())
	must(t, err)
	t.Cleanup(pool.Close)
	return pool, u.String()
}

func (e *environment) count(pool *pgxpool.Pool, query string, args ...any) int {
	e.t.Helper()
	var n int
	must(e.t, pool.QueryRow(e.ctx, query, args...).Scan(&n))
	return n
}
func (e *environment) jsonRow(pool *pgxpool.Pool, query string, out any, args ...any) bool {
	e.t.Helper()
	var raw []byte
	err := pool.QueryRow(e.ctx, query, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	must(e.t, err)
	must(e.t, json.Unmarshal(raw, out))
	return true
}

// Ark 可以运行较久；刷新按真实认证时间进行，与业务日期前拨无关。
func (e *environment) ensureAuth() {
	if e.token == "" || time.Until(e.accessExpires) > time.Minute {
		return
	}
	code, raw := request(e.ctx, e.bff.server.URL+"/v1/auth/refresh", "POST", "", map[string]string{"refresh_token": e.refreshToken})
	if code != 200 {
		e.t.Fatal("application credential refresh failed")
	}
	var out struct {
		Access  string    `json:"access_token"`
		Refresh string    `json:"refresh_token"`
		Expires time.Time `json:"access_expires_at"`
	}
	must(e.t, json.Unmarshal(raw, &out))
	e.token, e.refreshToken, e.accessExpires = out.Access, out.Refresh, out.Expires
}
