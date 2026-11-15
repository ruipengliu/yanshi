package day

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/outbox"
	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
	"yanshi/sdk/nodesdk"
)

type failoverReport struct {
	Kind         string         `json:"kind"`
	Passed       bool           `json:"passed"`
	ElapsedMS    int64          `json:"elapsed_ms"`
	WindowMS     int64          `json:"switch_window_ms"`
	RecoveryMS   int64          `json:"recovery_ms"`
	Requests     map[string]int `json:"window_requests"`
	MaxRequestMS int64          `json:"max_request_ms"`
	mu           sync.Mutex
	active       bool
}

func (r *failoverReport) record(kind string, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return
	}
	r.Requests[kind]++
	r.MaxRequestMS = max(r.MaxRequestMS, duration.Milliseconds())
}

func writeResult(t *testing.T, name string, result any) {
	t.Helper()
	switch rows := result.(type) {
	case *failoverReport:
		rows.mu.Lock()
		defer rows.mu.Unlock()
	case []*failoverReport:
		for _, row := range rows {
			row.mu.Lock()
			defer row.mu.Unlock()
		}
	}
	path := filepath.Join("../..", "eval-results", "failover", name+".json")
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	raw, err := json.MarshalIndent(result, "", "  ")
	must(t, err)
	must(t, os.WriteFile(path, append(raw, '\n'), 0600))
	t.Logf("result=%s", path)
}

func failoverDSNs(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("YANSHI_TEST_FAILOVER") != "1" && os.Getenv("YANSHI_TEST_CAPACITY") != "1" {
		t.Skip("use make failover or make shard-capacity; never uses shared PostgreSQL")
	}
	primary, standby := os.Getenv("YANSHI_FAILOVER_PRIMARY"), os.Getenv("YANSHI_FAILOVER_STANDBY")
	// 防止直接 go test 继承共享开发地址；控制命令只能操作本文件对应的项目。
	for i, dsn := range []string{primary, standby, os.Getenv("YANSHI_TEST_PG")} {
		cfg, err := pgx.ParseConfig(dsn)
		must(t, err)
		if cfg.Host != "127.0.0.1" || cfg.Port != uint16(55430+i) || cfg.Database != "yanshi" {
			t.Fatal("failover requires isolated loopback ports 55430/55431/55432")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, primary)
	must(t, err)
	defer conn.Close(context.Background())
	for {
		var ready bool
		must(t, conn.QueryRow(ctx, `SELECT current_setting('synchronous_commit')='on'
			AND current_setting('synchronous_standby_names')='FIRST 1 (failover_standby)'
			AND EXISTS (SELECT 1 FROM pg_stat_replication WHERE application_name='failover_standby' AND sync_state='sync' AND state='streaming')`).Scan(&ready))
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("synchronous standby not ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return primary, standby
}

func composeFailover(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", "yanshi-failover", "-f", "../../deploy/failover/compose.yaml"}, args...)...)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("isolated Compose operation failed")
	}
	return raw
}

// 关闭唯一应用入口后提升，确认旧主停机后才开放新主；期间健康片仍可运行。
func switchDatabase(t *testing.T, proxy *tcpProxy, standby string, during func(), report *failoverReport) {
	t.Helper()
	started := time.Now()
	report.mu.Lock()
	report.active = true
	report.mu.Unlock()
	proxy.cut(true)
	// during 中的 Fatal/Goexit 也要先完成提升与 fencing，随后 schema 清理才能通过原代理到新主。
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, standby)
		must(t, err)
		defer conn.Close(context.Background())
		var promoted bool
		must(t, conn.QueryRow(ctx, `SELECT pg_promote(true, 20)`).Scan(&promoted))
		if !promoted {
			t.Fatal("standby not promoted")
		}
		composeFailover(t, "stop", "-t", "1", "primary")
		if len(composeFailover(t, "ps", "--status", "running", "-q", "primary")) != 0 {
			t.Fatal("old primary still running")
		}
		var recovery bool
		must(t, conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&recovery))
		if recovery {
			t.Fatal("new primary remains in recovery")
		}
		proxy.switchTo(t, standby)
		report.mu.Lock()
		report.active = false
		report.mu.Unlock()
		report.WindowMS = time.Since(started).Milliseconds()
	}()
	during()
}

// FileLedger 在本次真实 Worker 调用中使用；恢复后重新构造 Executor 再投同一调用。
// 适配器只替代设备传输，不替代 Executor 的 started/done 持久顺序。
type effectProbe struct {
	ledger   nodesdk.FileLedger
	executor *nodesdk.Executor
	mu       sync.Mutex
	calls    map[string]*v1.Invoke
	counts   map[string]int
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newEffectProbe(t *testing.T) *effectProbe {
	p := &effectProbe{ledger: nodesdk.FileLedger{Dir: t.TempDir()}, calls: map[string]*v1.Invoke{}, counts: map[string]int{}}
	p.executor = p.newExecutor()
	return p
}
func (p *effectProbe) newExecutor() *nodesdk.Executor {
	return nodesdk.NewExecutor(p.ledger, nodesdk.Capability{Spec: &v1.CapabilitySpec{Name: "failover_probe"}, Handler: func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
		inv := nodesdk.Invocation(ctx)
		record, err := p.ledger.Get(inv.GetCallId())
		if err != nil || record.State != nodesdk.StateStarted {
			return nil, fmt.Errorf("ledger must persist started before effect")
		}
		p.mu.Lock()
		p.calls[inv.GetCallId()] = proto.Clone(inv).(*v1.Invoke)
		p.counts[inv.GetCallId()]++
		p.mu.Unlock()
		return model.TextBlocks("done"), nil
	}})
}
func (p *effectProbe) capability() capability.Capability {
	return capability.Func{S: capability.Spec{Name: "failover_probe", Risk: v1.Risk_RISK_LOW, Idempotent: true, InputSchema: json.RawMessage(`{"type":"object"}`)}, Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
		res, err := p.executor.Execute(ctx, &v1.Invoke{SessionId: inv.SessionID, RunId: inv.RunID, CallId: inv.CallID, Capability: "failover_probe", ArgumentsJson: inv.Arguments})
		if err != nil {
			return nil, err
		}
		if p.entered != nil {
			p.once.Do(func() { close(p.entered) })
			select {
			case <-p.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if res == nil || res.GetIsError() {
			return nil, fmt.Errorf("executor result unavailable")
		}
		return res.GetContent(), nil
	}}
}
func (p *effectProbe) verify(t *testing.T) {
	p.mu.Lock()
	calls := make([]*v1.Invoke, 0, len(p.calls))
	for _, inv := range p.calls {
		calls = append(calls, inv)
	}
	p.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("no side effect executed")
	}
	restarted := p.newExecutor()
	for _, inv := range calls {
		res, err := restarted.Execute(context.Background(), inv)
		must(t, err)
		if res == nil || res.GetIsError() {
			t.Fatal("ledger replay failed")
		}
		p.mu.Lock()
		n := p.counts[inv.GetCallId()]
		p.mu.Unlock()
		if n != 1 {
			t.Fatal("side effect executed more than once")
		}
	}
}

func (e *environment) kernelRequest(method, path string, body any, report *failoverReport) (int, json.RawMessage) {
	ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	return observedRequest(ctx, e.kernel.URL+path, method, e.serviceToken, body, report)
}
func (e *environment) createOnShard(owner string, wanted int) string {
	for range 64 {
		code, raw := e.kernelRequest("POST", "/v1/sessions", map[string]string{"end_user": owner, "agent": "xinli-assistant"}, nil)
		if code == 503 {
			continue
		}
		if code != 201 {
			e.t.Fatalf("create Session: %d", code)
		}
		var created struct {
			ID string `json:"session_id"`
		}
		must(e.t, json.Unmarshal(raw, &created))
		id, err := e.locator.Locate(shard.Target{Kind: shard.SessionData, Key: created.ID})
		must(e.t, err)
		if id == wanted {
			return created.ID
		}
	}
	e.t.Fatal("Session distribution did not cover shard")
	return ""
}

func TestFailoverKernel(t *testing.T) {
	if os.Getenv("YANSHI_TEST_FAILOVER") != "1" {
		t.Skip("set YANSHI_TEST_FAILOVER=1; use make failover")
	}
	primary, standby := failoverDSNs(t)
	report := &failoverReport{Kind: "kernel", Requests: map[string]int{}}
	started := time.Now()
	t.Cleanup(func() {
		report.ElapsedMS = time.Since(started).Milliseconds()
		report.Passed = !t.Failed()
		writeResult(t, "kernel", report)
	})
	e := newEnvironmentConfigured(t, false, true, "", []string{pgtest.Database(t), primary})
	e.start("orchestrator")
	bad := e.createOnShard(e.endUser, 1)
	owner := ""
	for i := 0; owner == ""; i++ {
		candidate := fmt.Sprintf("healthy-%d", i)
		if shard.Hash(candidate) < 512 {
			owner = candidate
		}
	}
	good := e.createOnShard(owner, 0)
	healthyCursor, err := e.core.Stores.Outbox.Head(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: 0})
	must(t, err)
	pool := e.shardPools[1]
	identity, schema, err := pg.DatabaseIdentity(e.ctx, pool)
	must(t, err)
	var epoch string
	must(t, pool.QueryRow(e.ctx, `SELECT stream_epoch FROM outbox_stream`).Scan(&epoch))
	// 先保留已确认 Event/信封，再让一个真实工具在结果写日志前跨越切换。
	run := e.kernelInput(bad, "call clock_now {}")
	e.run(bad, run, "completed")
	e.waitSent(run)
	prefix, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, bad, 0)
	must(t, err)
	envelopes, err := e.core.Stores.Outbox.List(e.ctx, businessLine)
	must(t, err)
	head, err := e.core.Stores.Outbox.Head(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: 1})
	must(t, err)
	blocker, err := pool.Begin(e.ctx)
	must(t, err)
	defer blocker.Rollback(context.Background())
	_, err = blocker.Exec(e.ctx, `SELECT pg_current_xact_id()`)
	must(t, err)
	blockedRun := e.kernelInput(bad, "call clock_now {}")
	e.run(bad, blockedRun, "completed")
	page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: 1, After: head})
	must(t, err)
	if len(page.Events) != 0 {
		t.Fatal("cursor crossed an open transaction")
	}
	probe := e.effects
	probe.entered, probe.release = make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-probe.release:
		default:
			close(probe.release)
		}
	}()
	inflight := e.kernelInput(bad, "call failover_probe {}")
	select {
	case <-probe.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("effect not reached")
	}
	n := e.shardNotifiers[1]
	ch, unsubscribe := n.Subscribe(pg.TopicEvents, "failover-listen-probe")
	defer unsubscribe()
	eventually(t, "LISTEN active", n.Ready)
	eventually(t, "initial LISTEN receives notification", func() bool {
		_, err := pool.Exec(e.ctx, `SELECT pg_notify($1,$2)`, n.Channel(pg.TopicEvents, "failover-listen-probe"), "failover-listen-probe")
		must(t, err)
		select {
		case <-ch:
			return true
		default:
			return false
		}
	})
	generation := n.Generation()
	prefix, err = eventlog.ReadAll(e.ctx, e.core.Stores.Log, bad, 0)
	must(t, err)
	// 排空初次 LISTEN 的提示；重连必须补提示，不能只等后续写入。
	for len(ch) > 0 {
		<-ch
	}
	unknownID := randomID(t)
	loadSession := e.createOnShard(e.endUser, 1)
	stopLoad := e.startInputLoad(loadSession, report)
	defer stopLoad()
	switchDatabase(t, e.shardProxies[1], standby, func() {
		for range 3 {
			code, _ := e.kernelRequest("POST", "/v1/sessions/"+bad+"/inputs", map[string]string{"input_id": unknownID, "text": "切换期间输入"}, report)
			if code != 503 {
				t.Fatalf("unavailable input must return retryable 503: %d", code)
			}
			code, raw := e.kernelRequest("POST", "/v1/sessions/"+good+"/inputs", map[string]string{"input_id": randomID(t), "text": "call clock_now {}"}, report)
			if code != 202 {
				t.Fatalf("healthy shard input: %d", code)
			}
			var submitted struct {
				ID string `json:"run_id"`
			}
			must(t, json.Unmarshal(raw, &submitted))
			e.run(good, submitted.ID, "completed")
			eventually(t, "healthy shard consumes completion during outage", func() bool {
				page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: 0, After: healthyCursor, Limit: 1000})
				must(t, err)
				healthyCursor = page.Next
				for _, envelope := range page.Events {
					if envelope.RunID == submitted.ID && envelope.Kind == outbox.RunCompleted {
						return true
					}
				}
				return false
			})
			fresh := e.createOnShard(owner, 0)
			code, _ = e.kernelRequest("DELETE", "/v1/sessions/"+fresh, nil, report)
			if code < 200 || code >= 300 {
				t.Fatalf("healthy delete: %d", code)
			}
		}
	}, report)
	recovery := time.Now()
	eventually(t, "pool and LISTEN reconnect", func() bool {
		ctx, cancel := context.WithTimeout(e.ctx, time.Second)
		defer cancel()
		return pool.Ping(ctx) == nil && n.Generation() > generation && n.Ready()
	})
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnected LISTEN did not wake subscriber")
	}
	for len(ch) > 0 {
		<-ch
	}
	_, err = pool.Exec(e.ctx, `SELECT pg_notify($1,$2)`, n.Channel(pg.TopicEvents, "failover-listen-probe"), "failover-listen-probe")
	must(t, err)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("new primary NOTIFY not delivered")
	}
	report.RecoveryMS = time.Since(recovery).Milliseconds()
	attempts := stopLoad()
	gotID, gotSchema, err := pg.DatabaseIdentity(e.ctx, pool)
	must(t, err)
	var gotEpoch string
	must(t, pool.QueryRow(e.ctx, `SELECT stream_epoch FROM outbox_stream`).Scan(&gotEpoch))
	if identity != gotID || schema != gotSchema || epoch != gotEpoch {
		t.Fatal("replication changed shard identity/schema/epoch")
	}
	close(probe.release)
	e.run(bad, inflight, "completed")
	code, raw := e.kernelRequest("POST", "/v1/sessions/"+bad+"/inputs", map[string]string{"input_id": unknownID, "text": "切换期间输入"}, nil)
	if code != 202 {
		t.Fatalf("retry input: %d", code)
	}
	code2, raw2 := e.kernelRequest("POST", "/v1/sessions/"+bad+"/inputs", map[string]string{"input_id": unknownID, "text": "切换期间输入"}, nil)
	var repeated struct {
		ID        string `json:"run_id"`
		Duplicate bool   `json:"duplicate"`
	}
	must(t, json.Unmarshal(raw2, &repeated))
	var initial struct {
		ID string `json:"run_id"`
	}
	must(t, json.Unmarshal(raw, &initial))
	if code2 != 202 || repeated.ID != initial.ID || !repeated.Duplicate {
		t.Fatal("input retry changed identity")
	}
	var submitted struct {
		ID string `json:"run_id"`
	}
	must(t, json.Unmarshal(raw, &submitted))
	e.run(bad, submitted.ID, "completed")
	after, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, bad, 0)
	must(t, err)
	if len(after) < len(prefix) {
		t.Fatal("confirmed events lost")
	}
	for i, event := range after {
		if event.GetSeq() != uint64(i+1) {
			t.Fatal("seq is not contiguous")
		}
		if i < len(prefix) && !proto.Equal(event, prefix[i]) {
			t.Fatal("confirmed event changed")
		}
	}
	rows, err := e.core.Stores.Outbox.List(e.ctx, businessLine)
	must(t, err)
	for _, old := range envelopes {
		found := false
		for _, current := range rows {
			if reflect.DeepEqual(old, current) {
				found = true
			}
		}
		if !found {
			t.Fatal("confirmed envelope lost")
		}
	}
	eventually(t, "old cursor reaches formerly blocked envelope", func() bool {
		page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: 1, After: head, Limit: 1000})
		must(t, err)
		for _, row := range page.Events {
			if row.RunID == blockedRun {
				return true
			}
		}
		return false
	})
	e.waitSent(inflight)
	e.waitSent(blockedRun)
	probe.verify(t)
	probe.mu.Lock()
	calls := make([]string, 0, len(probe.calls))
	for id := range probe.calls {
		calls = append(calls, id)
	}
	probe.mu.Unlock()
	for _, id := range calls {
		results := 0
		for _, event := range after {
			if result := event.GetToolResult(); result != nil && result.GetCallId() == id {
				if result.GetIsError() {
					t.Fatal("recovered tool result failed")
				}
				results++
			}
		}
		if results != 1 {
			t.Fatal("tool result was lost or duplicated")
		}
	}
	e.verifyInputLoad(loadSession, attempts)
	e.verifyServeIdentity()
	code, _ = e.kernelRequest("DELETE", "/v1/sessions/"+bad, nil, nil)
	if code < 200 || code >= 300 {
		t.Fatal("delete after failover failed")
	}
	eventually(t, "Janitor resumes cleanup", func() bool {
		events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, bad, 0)
		return err == nil && len(events) == 0
	})
}

// 使用生产 serve 的只读身份/迁移核对与 /readyz，不以测试自己的 SELECT 替代启动核对。
func (e *environment) verifyServeIdentity() {
	path := filepath.Join(e.t.TempDir(), "shards.json")
	dbs := []map[string]any{}
	for id := range 2 {
		cfg := e.shardPools[id].Config().ConnConfig
		u, err := url.Parse(cfg.ConnString())
		must(e.t, err)
		q := u.Query()
		q.Set("search_path", cfg.RuntimeParams["search_path"])
		u.RawQuery = q.Encode()
		dbs = append(dbs, map[string]any{"id": id, "sql_dsn": u.String(), "direct_dsn": u.String()})
	}
	raw, err := json.Marshal(map[string]any{"table": map[string]any{"shards": []int{0, 1}, "ranges": []map[string]int{{"first": 0, "last": 511, "shard": 0}, {"first": 512, "last": 1023, "shard": 1}}}, "version": "day-dual", "databases": dbs})
	must(e.t, err)
	must(e.t, os.WriteFile(path, raw, 0600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(e.t, err)
	address := listener.Addr().String()
	must(e.t, listener.Close())
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(e.root, "bin/yanshi"), "serve", "-storage", "postgres", "-shards", path, "-migrate=false", "-roles", "gateway", "-addr", address, "-auth", "none", "-agents", filepath.Join(e.root, "agents"), "-oneshots", filepath.Join(e.t.TempDir(), "absent"), "-mcp", filepath.Join(e.t.TempDir(), "absent"))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	must(e.t, cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() { cancel(); <-done }()
	eventually(e.t, "serve identity verification", func() bool {
		select {
		case <-done:
			e.t.Fatal("serve exited before identity verification")
		default:
		}
		ctx, cancel := context.WithTimeout(e.ctx, time.Second)
		defer cancel()
		code, raw := request(ctx, "http://"+address+"/readyz", "GET", "", nil)
		if code != 200 {
			return false
		}
		var state struct {
			Shards []struct {
				Identity   bool `json:"identity"`
				SQL        bool `json:"sql"`
				Migrations bool `json:"migrations"`
				Listen     bool `json:"listen"`
			} `json:"shards"`
		}
		if json.Unmarshal(raw, &state) != nil || len(state.Shards) != 2 {
			return false
		}
		for _, s := range state.Shards {
			if !s.Identity || !s.SQL || !s.Migrations || !s.Listen {
				return false
			}
		}
		return true
	})
}

func (e *environment) kernelInput(sid, text string) string {
	code, raw := e.kernelRequest("POST", "/v1/sessions/"+sid+"/inputs", map[string]string{"input_id": randomID(e.t), "text": text}, nil)
	if code != 202 {
		e.t.Fatalf("kernel input: %d", code)
	}
	var submitted struct {
		ID string `json:"run_id"`
	}
	must(e.t, json.Unmarshal(raw, &submitted))
	return submitted.ID
}

type inputAttempt struct {
	ID, Run string
	Status  int
}

// 相同 ID 的未知结果留给恢复后对账；负载线程只收集状态，不在后台调用 Fatal。
func (e *environment) startInputLoad(sid string, report *failoverReport) func() []inputAttempt {
	stop, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var attempts []inputAttempt
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			id := fmt.Sprintf("failover-load-%d", i)
			code, raw := e.kernelRequest("POST", "/v1/sessions/"+sid+"/inputs", map[string]string{"input_id": id, "text": "持续输入"}, report)
			var reply struct {
				Run string `json:"run_id"`
			}
			_ = json.Unmarshal(raw, &reply)
			attempts = append(attempts, inputAttempt{id, reply.Run, code})
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
			}
		}
	}()
	return func() []inputAttempt { once.Do(func() { close(stop) }); <-done; return attempts }
}

func (e *environment) verifyInputLoad(sid string, attempts []inputAttempt) {
	if len(attempts) == 0 {
		e.t.Fatal("no continuous load")
	}
	before, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
	must(e.t, err)
	known := map[string]string{}
	for _, event := range before {
		if input := event.GetRunRequested(); input != nil {
			known[input.GetInputId()] = input.GetRunId()
		}
		if input := event.GetSteered(); input != nil {
			known[input.GetInputId()] = input.GetRunId()
		}
	}
	for _, attempt := range attempts {
		if attempt.Status != 202 && attempt.Status != 503 && attempt.Status != 0 {
			e.t.Fatalf("unexpected load status: %d", attempt.Status)
		}
		// 平台近期去重窗口有界；先对账完整日志，不能把任意久远的重投当永久幂等。
		if run, ok := known[attempt.ID]; ok {
			if attempt.Status == 202 && run != attempt.Run {
				e.t.Fatal("confirmed input changed Run")
			}
			continue
		}
		if attempt.Status == 202 {
			e.t.Fatal("confirmed input lost")
		}
		code, _ := e.kernelRequest("POST", "/v1/sessions/"+sid+"/inputs", map[string]string{"input_id": attempt.ID, "text": "持续输入"}, nil)
		if code != 202 {
			e.t.Fatal("input replay did not recover")
		}
	}
	events, err := eventlog.ReadAll(e.ctx, e.core.Stores.Log, sid, 0)
	must(e.t, err)
	counts := map[string]int{}
	for _, event := range events {
		if input := event.GetRunRequested(); input != nil {
			counts[input.GetInputId()]++
		}
		if input := event.GetSteered(); input != nil {
			counts[input.GetInputId()]++
		}
	}
	for _, attempt := range attempts {
		if counts[attempt.ID] != 1 {
			e.t.Fatal("input replay duplicated or lost event")
		}
	}
}

// 将传输断开与截止分别计数；正文只在内存中用于断言，报告不保存。
func observedRequest(ctx context.Context, address, method, token string, in any, report *failoverReport) (int, json.RawMessage) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, nil
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	at := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		kind := "network"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind = "timeout"
		}
		if report != nil {
			report.record(kind, time.Since(at))
		}
		return 0, nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		if report != nil {
			kind := "network"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				kind = "timeout"
			}
			report.record(kind, time.Since(at))
		}
		return 0, nil
	}
	if report != nil {
		report.record("http_"+strconv.Itoa(resp.StatusCode), time.Since(at))
	}
	return resp.StatusCode, raw
}
