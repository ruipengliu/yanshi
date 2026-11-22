package pglog_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/ids"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/session"
	"yanshi/internal/session/pgsnapshot"
)

// 压测会向 PostgreSQL 写入大量数据，默认跳过以保持 make check 的耗时。
// 在 Mac mini 开发环境中运行（省略 -race，避免干扰延迟）：
//
//	dev run 'YANSHI_BENCH_TOOLRESULT=1 go test -v -count=1 -run "^TestToolResultSizeProfile$" ./internal/eventlog/pglog'
//
// dev 提供 YANSHI_TEST_PG。可用 YANSHI_BENCH_OUT 指定 JSON 输出路径。
func TestToolResultSizeProfile(t *testing.T) {
	if os.Getenv("YANSHI_BENCH_TOOLRESULT") != "1" {
		t.Skip("需要 YANSHI_BENCH_TOOLRESULT=1")
	}
	pgtest.DSN(t)

	var rows []toolResultSizeRow
	for _, kib := range []int{1, 16, 64, 256, 1024} {
		n := 100
		if kib > 64 {
			n = 20
		}
		for _, payload := range []string{"entropy", "json"} {
			t.Run(fmt.Sprintf("%dKiB/%s", kib, payload), func(t *testing.T) {
				rows = append(rows, profileToolResultSize(t, kib*1024, n, payload))
			})
		}
	}
	if t.Failed() {
		return
	}

	var table strings.Builder
	const line = "%6s %-7s %3s %13s %17s %19s %19s %19s %15s %19s %15s %15s %19s\n"
	fmt.Fprintf(&table, line, "KiB", "Payload", "N", "p50/p95 ms", "Append ms", "AppendWAL KiB", "Events KiB", "TOAST KiB",
		"Load ms", "Snap KiB", "SnapPut ms", "Delete ms", "DelWAL KiB")
	for _, r := range rows {
		fmt.Fprintf(&table, line, fmt.Sprint(r.PayloadBytes/1024), r.Payload, fmt.Sprint(r.Results),
			fmt.Sprintf("%.3f/%.3f", r.AppendP50MS, r.AppendP95MS), r.AppendMS.format(1),
			r.AppendWALBytes.format(1024), r.EventsBytes.format(1024), r.TOASTBytes.format(1024), r.LoadMS.format(1),
			r.SnapshotBytes.format(1024), r.SnapshotPutMS.format(1), r.DeleteMS.format(1), r.DeleteWALBytes.format(1024))
	}
	t.Logf("ToolResult 提交延迟为 p50/p95，其余度量为总量/每条结果。Events 已含 TOAST，N/A 表示 WAL 不可用。\n%s", table.String())

	if path := os.Getenv("YANSHI_BENCH_OUT"); path != "" {
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type toolResultSizeRow struct {
	Payload        string             `json:"payload"`
	PayloadBytes   int                `json:"payload_bytes"`
	Results        int                `json:"results"`
	AppendP50MS    float64            `json:"tool_result_append_p50_ms"`
	AppendP95MS    float64            `json:"tool_result_append_p95_ms"`
	AppendMS       toolResultMeasure  `json:"append_ms"`
	AppendWALBytes *toolResultMeasure `json:"append_wal_bytes"`
	EventsBytes    toolResultMeasure  `json:"events_bytes"`
	TOASTBytes     toolResultMeasure  `json:"toast_bytes"`
	LoadMS         toolResultMeasure  `json:"load_ms"`
	SnapshotBytes  toolResultMeasure  `json:"snapshot_bytes"`
	SnapshotPutMS  toolResultMeasure  `json:"snapshot_put_ms"`
	DeleteMS       toolResultMeasure  `json:"delete_ms"`
	DeleteWALBytes *toolResultMeasure `json:"delete_wal_bytes"`
}

type toolResultMeasure struct {
	Total     float64 `json:"total"`
	PerResult float64 `json:"per_result"`
}

func toolResultPerResult(total float64, n int) toolResultMeasure {
	return toolResultMeasure{Total: total, PerResult: total / float64(n)}
}

func (m *toolResultMeasure) format(unit float64) string {
	if m == nil {
		return "N/A"
	}
	return fmt.Sprintf("%.3f/%.3f", m.Total/unit, m.PerResult/unit)
}

func profileToolResultSize(t *testing.T, size, n int, payload string) toolResultSizeRow {
	t.Helper()
	ctx := t.Context()
	// 各格独享关系文件，避免上一格留下的页影响体积差。
	pool, notifier := pgtest.Fresh(t)
	log := pglog.New(pool, notifier)
	snapshots := pgsnapshot.Store{Pool: pool}
	sid, events := toolResultSizeEvents(t, size, n, payload)
	if err := (&session.State{}).Check(events...); err != nil {
		t.Fatal(err)
	}
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	row := toolResultSizeRow{Payload: payload, PayloadBytes: size, Results: n}
	beforeEvents, beforeTOAST := toolResultRelationSizes(t, pool)
	latencies := make([]time.Duration, 0, n)
	var seq uint64
	row.AppendWALBytes = measureToolResultWAL(t, pool, n, func() {
		start := time.Now()
		for _, e := range events {
			appendStart := time.Now()
			next, err := log.Append(ctx, sid, seq, nil, e)
			elapsed := time.Since(appendStart)
			if err != nil {
				t.Fatalf("追加事件 %d: %v", seq+1, err)
			}
			seq = next
			if e.GetToolResult() != nil {
				latencies = append(latencies, elapsed)
			}
		}
		row.AppendMS = toolResultPerResult(ms(time.Since(start)), n)
	})
	if len(latencies) != n {
		t.Fatalf("追加了 %d 条 ToolResult，预期 %d 条", len(latencies), n)
	}
	slices.Sort(latencies)
	// 用最近秩法取分位，便于解释 N=20 时的结果。
	row.AppendP50MS = ms(latencies[(n*50+99)/100-1])
	row.AppendP95MS = ms(latencies[(n*95+99)/100-1])

	afterEvents, afterTOAST := toolResultRelationSizes(t, pool)
	row.EventsBytes = toolResultPerResult(float64(afterEvents-beforeEvents), n)
	row.TOASTBytes = toolResultPerResult(float64(afterTOAST-beforeTOAST), n)

	// 从空投影重放，保留 PostgreSQL 和操作系统的正常页缓存。
	start := time.Now()
	loaded, err := eventlog.ReadAll(ctx, log, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := session.Reduce(loaded)
	row.LoadMS = toolResultPerResult(ms(time.Since(start)), n)
	if err != nil {
		t.Fatal(err)
	}
	if state.Seq != seq {
		t.Fatalf("重放末尾为 %d，预期 %d", state.Seq, seq)
	}

	// 独立度量完整历史的快照写入，Put 计时含 protobuf 编码。
	snap := state.Snapshot()
	if snap == nil {
		t.Fatal("无法为完整投影生成快照")
	}
	start = time.Now()
	err = snapshots.Put(ctx, snap)
	row.SnapshotPutMS = toolResultPerResult(ms(time.Since(start)), n)
	if err != nil {
		t.Fatal(err)
	}
	row.SnapshotBytes = toolResultPerResult(float64(proto.Size(snap)), n)

	// Delete 只度量 Event 日志存储层，隔离 schema 的清理会一并移除快照。
	row.DeleteWALBytes = measureToolResultWAL(t, pool, n, func() {
		start := time.Now()
		err := log.Delete(ctx, sid)
		row.DeleteMS = toolResultPerResult(ms(time.Since(start)), n)
		if err != nil {
			t.Fatal(err)
		}
	})
	return row
}

func toolResultRelationSizes(t *testing.T, pool *pgxpool.Pool) (events, toast int64) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `
		SELECT pg_total_relation_size(oid::regclass), pg_total_relation_size(reltoastrelid::regclass)
		FROM pg_catalog.pg_class WHERE oid = 'events'::regclass`).Scan(&events, &toast)
	if err != nil {
		t.Fatal(err)
	}
	return events, toast
}

func measureToolResultWAL(t *testing.T, pool *pgxpool.Pool, n int, write func()) *toolResultMeasure {
	t.Helper()
	// LSN 是实例级计数，测量时需要让同实例的其他写入方空闲。
	var before string
	err := pool.QueryRow(t.Context(), `SELECT pg_current_wal_lsn()::text`).Scan(&before)
	write()
	var delta int64
	if err == nil {
		err = pool.QueryRow(t.Context(), `
			SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::text::pg_lsn)::bigint`, before).Scan(&delta)
	}
	if err == nil {
		m := toolResultPerResult(float64(delta), n)
		return &m
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		t.Logf("WAL 不可用，权限不足: %v", err)
		return nil
	}
	t.Fatalf("读取 WAL 位置: %v", err)
	return nil
}

func toolResultSizeEvents(t *testing.T, size, n int, payload string) (string, []*v1.Event) {
	t.Helper()
	const endUser = "size-profile"
	const attempt = 1
	newID := ids.Random()
	sid, err := newID.Session(endUser)
	if err != nil {
		t.Fatal(err)
	}
	forSession := func(prefix string) string {
		id, err := newID.ForSession(prefix, sid)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	runID := forSession("run")
	events := []*v1.Event{
		{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bench", EndUser: endUser}}},
		{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: runID}}},
		{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: runID, Attempt: attempt}}},
	}
	// 固定随机种子便于复现，每条结果从不同位置取样，避免重放同一份正文。
	rng := rand.New(rand.NewPCG(42, 1))
	for range n {
		callID := forSession("call")
		message := &v1.AssistantMessage{RunId: runID, Attempt: attempt, ToolCalls: []*v1.ToolCall{
			{CallId: callID, Capability: "bench", ArgumentsJson: "{}"},
		}}
		started := &v1.ToolCallStarted{RunId: runID, Attempt: attempt, CallId: callID}
		body := &v1.Text{Text: toolResultSizePayload(rng, size, payload)}
		result := &v1.ToolResult{RunId: runID, Attempt: attempt, CallId: callID, Content: []*v1.ContentBlock{
			{Kind: &v1.ContentBlock_Text{Text: body}},
		}}
		events = append(events,
			&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: message}},
			&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: started}},
			&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: result}},
		)
	}
	now := timestamppb.Now()
	for _, e := range events {
		e.Id, e.SessionId, e.Time = newID(), sid, now
	}
	return sid, events
}

func toolResultSizePayload(rng *rand.Rand, size int, payload string) string {
	var b strings.Builder
	b.Grow(size)
	if payload == "entropy" {
		for range size {
			b.WriteByte(byte(' ' + rng.IntN(95)))
		}
		return b.String()
	}

	// 模拟列表查询的 JSON，键名重复而 ID 变化。尾部最多补一条记录长度的空白。
	const record = `{"id":"%08x","status":"completed","name":"inventory","quantity":12}`
	available := size - 1
	recordBytes := len(fmt.Sprintf(record, uint32(0))) + 1
	b.WriteByte('[')
	for i := range available / recordBytes {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, record, rng.Uint32())
	}
	b.WriteByte(']')
	b.WriteString(strings.Repeat(" ", size-b.Len()))
	return b.String()
}
