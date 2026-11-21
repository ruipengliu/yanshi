package day

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"yanshi/internal/outbox"
	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/shard"
)

// moveReport 是整片搬迁演练的结果（docs/design/deployment.md §10.5）。只含计时、计数、摘要与分片号，
// 不含原始 EndUser、输入、DSN 或游标（ADR-0051 §5.4 的迁移制品）。
type moveReport struct {
	failoverReport
	Shard          int          `json:"shard"`
	BallastGB      int          `json:"ballast_gb"`
	ClusterBytes   int64        `json:"cluster_bytes"`
	CopyMS         int64        `json:"copy_ms"`    // 在线基础备份到新主机开始流复制（入口开放）
	CatchupMS      int64        `json:"catchup_ms"` // 切到同步到 sync_state=sync（入口开放）
	DigestMS       int64        `json:"digest_ms"`  // 停写窗口内比对两端行数与内容摘要的耗时（已含在 switch_window_ms 中）
	HealthyOps     int          `json:"healthy_ops_in_window"`
	CopyOps        int          `json:"moving_shard_runs_during_copy"` // 基础备份期间分片 1 逐个完成的 Run
	Tables         []tableCheck `json:"tables"`
	IdentitySame   bool         `json:"shard_identity_unchanged"`
	EpochSame      bool         `json:"outbox_epoch_unchanged"`
	RoutingVersion string       `json:"routing_version"`
	RoutingDigest  string       `json:"routing_digest"`
}

type tableCheck struct {
	Table  string `json:"table"`
	Rows   int64  `json:"rows"`
	Digest string `json:"digest"`
	Equal  bool   `json:"equal"`
}

func moveDSNs(t *testing.T) (primary, newhost string) {
	t.Helper()
	if os.Getenv("YANSHI_TEST_FAILOVER") != "1" {
		t.Skip("use make shard-move; never uses shared PostgreSQL")
	}
	primary, newhost = os.Getenv("YANSHI_FAILOVER_PRIMARY"), os.Getenv("YANSHI_FAILOVER_NEWHOST")
	for dsn, port := range map[string]uint16{primary: 55430, newhost: 55433, os.Getenv("YANSHI_TEST_PG"): 55432} {
		cfg, err := pgx.ParseConfig(dsn)
		must(t, err)
		if cfg.Host != "127.0.0.1" || cfg.Port != port || cfg.Database != "yanshi" {
			t.Fatal("shard move requires isolated loopback ports 55430/55432/55433")
		}
	}
	return primary, newhost
}

func admin(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	must(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// fillBallast 在主库实例的独立 database 中写入约 gb GB 数据：它只让基础备份与追平有真实的体量，
// 不进入内核表，也不参与内容摘要。
func fillBallast(t *testing.T, primary string, gb int) {
	t.Helper()
	if gb <= 0 {
		return
	}
	conn := admin(t, primary)
	ctx := context.Background()
	_, err := conn.Exec(ctx, `CREATE DATABASE move_ballast`)
	must(t, err)
	cfg, err := pgx.ParseConfig(primary)
	must(t, err)
	cfg.Database = "move_ballast"
	b, err := pgx.ConnectConfig(ctx, cfg)
	must(t, err)
	defer b.Close(ctx)
	_, err = b.Exec(ctx, `CREATE TABLE ballast (id bigint, payload text) WITH (autovacuum_enabled = false)`)
	must(t, err)
	// 每行约 1KB（低于 TOAST 的压缩阈值，按原大小落盘），每批约 256MB。
	for i := range gb * 4 {
		_, err := b.Exec(ctx, `INSERT INTO ballast SELECT g, repeat(md5(g::text || random()::text), 30)
			FROM generate_series($1::bigint, $1::bigint + 262143) g`, int64(i)*262144)
		must(t, err)
	}
}

// tableDigests 返回一个 schema 下每张表的行数与内容摘要（按行文本排序后的 MD5），另加 public.shard_identity。
func tableDigests(t *testing.T, conn *pgx.Conn, schema string) map[string]tableCheck {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.Query(ctx, `SELECT table_schema, table_name FROM information_schema.tables
		WHERE (table_schema = $1 OR (table_schema = 'public' AND table_name = 'shard_identity')) AND table_type = 'BASE TABLE'
		ORDER BY 1, 2`, schema)
	must(t, err)
	var names [][2]string
	for rows.Next() {
		var s, n string
		must(t, rows.Scan(&s, &n))
		names = append(names, [2]string{s, n})
	}
	must(t, rows.Err())
	out := map[string]tableCheck{}
	for _, n := range names {
		q := pgx.Identifier{n[0], n[1]}.Sanitize()
		var c tableCheck
		c.Table = n[1]
		must(t, conn.QueryRow(ctx, `SELECT count(*), md5(coalesce(string_agg(r::text, E'\n' ORDER BY r::text), '')) FROM `+q+` r`).Scan(&c.Rows, &c.Digest))
		out[n[0]+"."+n[1]] = c
	}
	return out
}

func waitSync(t *testing.T, conn *pgx.Conn, name string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		var ok bool
		must(t, conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_replication WHERE application_name=$1 AND sync_state='sync' AND state='streaming')`, name).Scan(&ok))
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("new host not synchronous")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestShardMoveKernel：把分片 1 整片搬到一台新主机（ADR-0051 §5.4）。主库起初没有备库、已有数据并持续服务；
// 新主机在线做基础备份、追平并切到同步；随后只冻结分片 1：关闭准入 → 比对两端摘要 → 提升 → 隔离旧主 → 改指端点。
// 断言 shard_identity、出站 epoch 与路由 Identity() 不变，各表行数与内容摘要一致，分片 0 持续服务，已确认写入不丢。
func TestShardMoveKernel(t *testing.T) {
	primary, newhost := moveDSNs(t)
	gb, _ := strconv.Atoi(os.Getenv("YANSHI_MOVE_BALLAST_GB"))
	report := &moveReport{failoverReport: failoverReport{Kind: "shard-move", Requests: map[string]int{}}, Shard: 1, BallastGB: gb}
	started := time.Now()
	t.Cleanup(func() {
		report.ElapsedMS = time.Since(started).Milliseconds()
		report.Passed = !t.Failed()
		writeResult(t, fmt.Sprintf("shard-move-%dg", gb), report)
	})
	src := admin(t, primary)
	ctx := context.Background()
	// 初始化脚本为主备演练登记了同步备库；搬迁开始前主库独自运行，先取消同步等待。
	_, err := src.Exec(ctx, `ALTER SYSTEM SET synchronous_standby_names = ''`)
	must(t, err)
	_, err = src.Exec(ctx, `SELECT pg_reload_conf()`)
	must(t, err)
	fillBallast(t, primary, gb)

	e := newEnvironmentConfigured(t, false, true, "", []string{pgtest.Database(t), primary})
	e.start("orchestrator")
	report.RoutingVersion, report.RoutingDigest = e.locator.Identity()
	moving := e.createOnShard(e.endUser, 1)
	owner := ""
	for i := 0; owner == ""; i++ {
		if c := fmt.Sprintf("healthy-%d", i); shard.Hash(c) < 512 {
			owner = c
		}
	}
	good := e.createOnShard(owner, 0)
	for range 3 { // 搬迁前的已确认数据：Run、工具结果与出站信封
		run := e.kernelInput(moving, "call clock_now {}")
		e.run(moving, run, "completed")
		e.waitSent(run)
	}
	pool := e.shardPools[1]
	identity, schema, err := pg.DatabaseIdentity(e.ctx, pool)
	must(t, err)
	var epoch string
	must(t, pool.QueryRow(e.ctx, `SELECT stream_epoch FROM outbox_stream`).Scan(&epoch))
	head, err := e.core.Stores.Outbox.Head(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: 1})
	must(t, err)
	loadSession := e.createOnShard(e.endUser, 1)

	// 在线建新主机：基础备份期间分片 1 照常服务（逐个完成 Run；高频输入负载放在备份之后，
	// 以免积压随备份时长增长、拖慢之后的出站核对）。
	must(t, src.QueryRow(ctx, `SELECT sum(pg_database_size(datname))::bigint FROM pg_database`).Scan(&report.ClusterBytes))
	copyDone := make(chan struct{})
	var copyOps sync.WaitGroup
	copyOps.Add(1)
	go func() {
		defer copyOps.Done()
		for {
			select {
			case <-copyDone:
				return
			default:
			}
			run := e.kernelInput(moving, "call clock_now {}")
			e.run(moving, run, "completed")
			report.CopyOps++
		}
	}()
	copyStart := time.Now()
	up := exec.Command("docker", "compose", "-p", "yanshi-failover", "-f", "../../deploy/failover/compose.yaml", "--profile", "move", "up", "-d", "--wait", "newhost")
	out, err := up.CombinedOutput()
	report.CopyMS = time.Since(copyStart).Milliseconds()
	close(copyDone)
	copyOps.Wait()
	if err != nil {
		t.Fatalf("start new host: %v: %s", err, out)
	}
	if report.CopyOps == 0 {
		t.Fatal("moving shard did not serve during the base backup")
	}
	stopLoad := e.startInputLoad(loadSession, &report.failoverReport)
	defer stopLoad()
	catchup := time.Now()
	_, err = src.Exec(ctx, `ALTER SYSTEM SET synchronous_standby_names = 'FIRST 1 (shard_move_target)'`)
	must(t, err)
	_, err = src.Exec(ctx, `SELECT pg_reload_conf()`)
	must(t, err)
	waitSync(t, src, "shard_move_target", 10*time.Minute)
	report.CatchupMS = time.Since(catchup).Milliseconds()
	dst := admin(t, newhost)

	// 停写窗口：只冻结分片 1；分片 0 的健康操作在窗口内并行进行。
	windowDone := make(chan struct{})
	var healthy sync.WaitGroup
	var healthyErr error
	healthy.Add(1)
	go func() {
		defer healthy.Done()
		for {
			select {
			case <-windowDone:
				return
			default:
			}
			code, raw := e.kernelRequest("POST", "/v1/sessions/"+good+"/inputs", map[string]string{"input_id": randomID(t), "text": "call clock_now {}"}, nil)
			if code != 202 {
				healthyErr = fmt.Errorf("healthy shard input during move: %d", code)
				return
			}
			var submitted struct {
				ID string `json:"run_id"`
			}
			if err := json.Unmarshal(raw, &submitted); err != nil {
				healthyErr = err
				return
			}
			e.run(good, submitted.ID, "completed")
			report.HealthyOps++
		}
	}()
	report.mu.Lock()
	report.active = true
	report.mu.Unlock()
	window := time.Now()
	e.shardProxies[1].cut(true)
	for range 2 {
		if code, _ := e.kernelRequest("POST", "/v1/sessions/"+moving+"/inputs", map[string]string{"input_id": randomID(t), "text": "冻结期间"}, &report.failoverReport); code != 503 {
			t.Fatalf("frozen shard must return retryable 503: %d", code)
		}
	}
	// 入口已关：等新主机重放到旧主的当前位置，再比对两端（新主机在恢复中只读）。
	var lsn string
	must(t, src.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn))
	eventually(t, "new host replayed to source position", func() bool {
		var caught bool
		must(t, dst.QueryRow(ctx, `SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn`, lsn).Scan(&caught))
		return caught
	})
	digest := time.Now()
	before, after := tableDigests(t, src, schema), tableDigests(t, dst, schema)
	report.DigestMS = time.Since(digest).Milliseconds()
	var promoted bool
	must(t, dst.QueryRow(ctx, `SELECT pg_promote(true, 60)`).Scan(&promoted))
	if !promoted {
		t.Fatal("new host not promoted")
	}
	composeFailover(t, "stop", "-t", "1", "primary")
	if len(composeFailover(t, "ps", "--status", "running", "-q", "primary")) != 0 {
		t.Fatal("old primary still running")
	}
	e.shardProxies[1].switchTo(t, newhost)
	report.WindowMS = time.Since(window).Milliseconds()
	report.mu.Lock()
	report.active = false
	report.mu.Unlock()
	close(windowDone)
	healthy.Wait()
	if healthyErr != nil {
		t.Fatal(healthyErr)
	}
	if report.HealthyOps == 0 {
		t.Fatal("healthy shard did not serve during the move")
	}
	for name, b := range before {
		a, ok := after[name]
		c := tableCheck{Table: name, Rows: b.Rows, Digest: b.Digest, Equal: ok && a == b}
		report.Tables = append(report.Tables, c)
		if !c.Equal {
			t.Errorf("table %s differs after move (rows %d → %d)", name, b.Rows, a.Rows)
		}
	}
	slices.SortFunc(report.Tables, func(a, b tableCheck) int { return strings.Compare(a.Table, b.Table) })
	if len(after) != len(before) {
		t.Errorf("table set changed: %d → %d", len(before), len(after))
	}

	recovery := time.Now()
	eventually(t, "pool reconnects to new primary", func() bool {
		ctx, cancel := context.WithTimeout(e.ctx, time.Second)
		defer cancel()
		return pool.Ping(ctx) == nil
	})
	report.RecoveryMS = time.Since(recovery).Milliseconds()
	gotID, gotSchema, err := pg.DatabaseIdentity(e.ctx, pool)
	must(t, err)
	var gotEpoch string
	must(t, pool.QueryRow(e.ctx, `SELECT stream_epoch FROM outbox_stream`).Scan(&gotEpoch))
	report.IdentitySame = identity == gotID && schema == gotSchema
	report.EpochSame = epoch == gotEpoch
	if !report.IdentitySame || !report.EpochSame {
		t.Fatal("move changed shard identity or outbox epoch")
	}
	if v, d := e.locator.Identity(); v != report.RoutingVersion || d != report.RoutingDigest {
		t.Fatal("move changed routing identity")
	}
	attempts := stopLoad()
	e.verifyInputLoad(loadSession, attempts)
	// 搬迁后：分片 1 的新 Run、出站消费（旧游标继续）、删除与生产 serve 的身份核验。
	run := e.kernelInput(moving, "call clock_now {}")
	e.run(moving, run, "completed")
	e.waitSent(run)
	eventually(t, "old cursor continues on new primary", func() bool {
		page, err := e.core.Stores.Outbox.Read(e.ctx, outbox.Request{BusinessLine: businessLine, Shard: 1, After: head, Limit: 1000})
		must(t, err)
		for _, row := range page.Events {
			if row.RunID == run {
				return true
			}
		}
		return false
	})
	e.verifyServeIdentity()
	if code, _ := e.kernelRequest("DELETE", "/v1/sessions/"+moving, nil, nil); code < 200 || code >= 300 {
		t.Fatalf("delete after move: %d", code)
	}
}
