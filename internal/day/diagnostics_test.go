package day

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/eventlog"
	"yanshi/internal/model"
)

// 验收必须固定影响进程生命周期与 A10 的开关，不能继承开发服务或评测的环境默认值。
func isolatedServeArgs(args []string) []string {
	return append(args, "-auto-schedule=true", "-exit-on-stdin-eof=false")
}

type httpObservation struct {
	Method, Path string
	Status       int
}
type httpTrace struct {
	mu     sync.Mutex
	counts map[httpObservation]int
}
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (h *httpTrace) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(status, r)
		if status.status == 0 {
			status.status = http.StatusOK
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		// 不留 query、请求/响应体或 Authorization；创建 Session 失败也能看到 HTTP 状态。
		if h.counts == nil {
			h.counts = map[httpObservation]int{}
		}
		key := httpObservation{r.Method, r.URL.Path, status.status}
		if _, known := h.counts[key]; !known && len(h.counts) >= 200 {
			key.Path = "/other"
		}
		h.counts[key]++
	})
}

// Write 可能被 exec 的复制协程并发调用；同时限制行数与单行长度，失败诊断不能无界积累。
type lineTail struct {
	mu      sync.Mutex
	lines   []string
	partial string
}

func (b *lineTail) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, part := range strings.SplitAfter(string(data), "\n") {
		b.partial += part
		if len(b.partial) > 16384 {
			b.partial = b.partial[:16384]
		}
		if strings.HasSuffix(part, "\n") {
			b.lines = append(b.lines, strings.TrimSuffix(b.partial, "\n"))
			if len(b.lines) > 200 {
				b.lines = b.lines[len(b.lines)-200:]
			}
			b.partial = ""
		}
	}
	return len(data), nil
}

func (b *lineTail) tail() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]string(nil), b.lines...)
	if b.partial != "" {
		out = append(out, b.partial)
	}
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	return out
}

var bearerSecret = regexp.MustCompile(`(?i)Bearer\s+[^\s"']+`)
var jwtSecret = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var urlSecret = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+@`)
var namedSecret = regexp.MustCompile(`(?i)(authorization|access_token|refresh_token|api_key|password|secret|token|dsn)([=:]\s*)("[^"]*"|[^\s,}]+)`)

func (e *environment) redact(value string) string {
	for _, entry := range e.environ {
		key, secret, ok := strings.Cut(entry, "=")
		if !ok || len(secret) < 4 {
			continue
		}
		key = strings.ToUpper(key)
		if strings.Contains(key, "TOKEN") || strings.Contains(key, "KEY") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD") || strings.Contains(key, "DSN") || strings.HasSuffix(key, "_CODE") {
			value = strings.ReplaceAll(value, secret, "<REDACTED>")
		}
	}
	for _, secret := range []string{e.token, e.refreshToken, e.serviceToken, e.control.token} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<REDACTED>")
		}
	}
	value = bearerSecret.ReplaceAllString(value, "Bearer <REDACTED>")
	value = jwtSecret.ReplaceAllString(value, "<REDACTED>")
	value = urlSecret.ReplaceAllString(value, "${1}<REDACTED>@")
	return namedSecret.ReplaceAllString(value, "${1}${2}<REDACTED>")
}

func (p *process) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type childClock struct {
	Now          time.Time `json:"now"`
	Offset       int64     `json:"offset_ns"`
	Reads        uint64    `json:"clock_reads"`
	AutoSchedule bool      `json:"auto_schedule"`
	ExitOnEOF    bool      `json:"exit_on_stdin_eof"`
}

func (e *environment) processClock(p *process) (json.RawMessage, bool) {
	if !p.running() || p.health == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, raw := request(ctx, p.health+"/debug/day", http.MethodGet, e.control.token, nil)
	return raw, code == http.StatusOK
}

func (e *environment) checkClock(p *process) {
	e.t.Helper()
	raw, ok := e.processClock(p)
	var clock childClock
	if !ok || json.Unmarshal(raw, &clock) != nil {
		e.t.Fatalf("role=%s pid=%d test clock unavailable", p.role, p.cmd.Process.Pid)
	}
	diff := e.control.Now().Sub(clock.Now)
	if diff < 0 {
		diff = -diff
	}
	if clock.Offset != e.control.offset.Load() || diff > 2*time.Second || !clock.AutoSchedule || clock.ExitOnEOF {
		e.t.Fatalf("role=%s pid=%d clock/config mismatch", p.role, p.cmd.Process.Pid)
	}
}

func (e *environment) diagnoseIfFailed() {
	if !e.t.Failed() {
		return
	}
	e.diagnosticOnce.Do(func() {
		e.t.Logf("day diagnostic parent_now=%s offset_ns=%d clock_mode=read_each_call test_scan_interval=100ms production_scan_interval=30s", e.control.Now().UTC().Format(time.RFC3339Nano), e.control.offset.Load())
		for _, p := range e.processHistory {
			state := "running"
			if !p.running() {
				state = "exited"
				if p.cmd.ProcessState != nil {
					state = p.cmd.ProcessState.String()
				}
			}
			e.t.Logf("day diagnostic role=%s pid=%d state=%s started_real=%s started_business=%s started_offset_ns=%d", p.role, p.cmd.Process.Pid, state, p.startedAt.Format(time.RFC3339Nano), p.startedBusiness.Format(time.RFC3339Nano), p.startedOffset)
			if raw, ok := e.processClock(p); ok {
				e.t.Logf("day diagnostic child_clock=%s", raw)
			} else {
				e.t.Logf("day diagnostic role=%s clock_probe=unavailable", p.role)
			}
			if p.role == "orchestrator" && p.running() && p.health != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				code, raw := request(ctx, p.health+"/debug/day?schedule=1", http.MethodGet, e.control.token, nil)
				cancel()
				if code == 200 {
					e.t.Logf("day diagnostic scheduler=%s", raw)
				} else {
					e.t.Logf("day diagnostic scheduler_probe_status=%d", code)
				}
			}
			lines := p.stderr.tail()
			e.t.Logf("day stderr BEGIN role=%s pid=%d lines=%d", p.role, p.cmd.Process.Pid, len(lines))
			if len(lines) == 0 {
				e.t.Log("day stderr (empty)")
			}
			for _, line := range lines {
				e.t.Logf("day stderr role=%s pid=%d %s", p.role, p.cmd.Process.Pid, e.redact(line))
			}
			e.t.Logf("day stderr END role=%s pid=%d", p.role, p.cmd.Process.Pid)
		}
		e.control.mu.Lock()
		for _, point := range []string{"scheduler.discovery_started", "scheduler.discovery_completed"} {
			e.t.Logf("day diagnostic %s=%s", point, e.control.diagnostics[point])
		}
		e.control.mu.Unlock()
		if e.kernelHTTP != nil {
			e.kernelHTTP.mu.Lock()
			counts := map[httpObservation]int{}
			for key, count := range e.kernelHTTP.counts {
				counts[key] = count
			}
			e.kernelHTTP.mu.Unlock()
			keys := make([]httpObservation, 0, len(counts))
			for key := range counts {
				keys = append(keys, key)
			}
			sort.Slice(keys, func(i, j int) bool {
				if keys[i].Method != keys[j].Method {
					return keys[i].Method < keys[j].Method
				}
				if keys[i].Path != keys[j].Path {
					return keys[i].Path < keys[j].Path
				}
				return keys[i].Status < keys[j].Status
			})
			for _, key := range keys {
				e.t.Logf("day kernel_http method=%s path=%s status=%d count=%d", key.Method, key.Path, key.Status, counts[key])
			}
		}
		e.dumpScript()
		// 只选身份、时刻与状态；绝不输出原文、任务 receipt 或账号凭据载荷。
		e.snapshot(e.appPool, "accounts", `SELECT jsonb_build_object('business_line',business_line,'end_user',end_user,'status',status) FROM accounts ORDER BY business_line,end_user LIMIT 200`)
		e.snapshot(e.personaPool, "scheduling_owners", `SELECT jsonb_build_object('business_line',convert_from(o.business_line,'UTF8'),'end_user',convert_from(o.end_user,'UTF8'),'deleting',EXISTS(SELECT 1 FROM owner_deletions d WHERE d.business_line=o.business_line AND d.end_user=o.end_user)) FROM owners o ORDER BY o.business_line,o.end_user LIMIT 200`)
		e.snapshot(e.appPool, "task_sources", `SELECT to_jsonb(s) FROM task_sources s ORDER BY received_at,source_id LIMIT 200`)
		e.snapshot(e.appPool, "orchestration_tasks", `SELECT jsonb_build_object('task_id',task_id,'source_id',source_id,'source_version',source_version,'source_kind',source_kind,'operation',operation,'target',target,'business_date',business_date,'state',state,'session_id',session_id,'input_id',input_id,'attempt',attempt,'retirements',retirements,'lease_generation',lease_generation,'lease_until',lease_until,'retry_at',retry_at,'command_started_at',command_started_at,'last_error_class',last_error_class) FROM orchestration_tasks ORDER BY sequence LIMIT 200`)
		e.snapshot(e.appPool, "session_creations", `SELECT jsonb_build_object('id',id,'task_id',task_id,'created_at',created_at,'scan_at',scan_at) FROM session_creations ORDER BY scan_at,id LIMIT 200`)
		e.snapshot(e.personaPool, "business_days", `SELECT jsonb_build_object('business_day',convert_from(business_day,'UTF8'),'last_batch_at',payload->'LastBatchAt','daytime_due_at',(payload->>'LastBatchAt')::timestamptz+interval '5 minutes','time_zone',payload->'TimeZone','capture_revision',payload->'CaptureRevision','tail',payload->'Tail','progress',payload->'Progress','read_through',payload->'ReadThrough','expires_at',expires_at,'cleanup_done',capture_cleanup_done,'expired',payload->'Expired','deleted',payload->'Deleted','session',payload->'Session','sessions',payload->'Sessions','delete_at',payload->'DeleteAt') FROM business_days ORDER BY business_day LIMIT 200`)
		e.snapshot(e.personaPool, "work_bindings", `SELECT jsonb_build_object('target',convert_from(target,'UTF8'),'version',payload->'Value'->'Version','lease',payload->'Value'->'Lease','delete_at',payload->'Value'->'DeleteAt') FROM work_bindings ORDER BY target LIMIT 200`)
		e.snapshot(e.personaPool, "work_sessions", `SELECT jsonb_build_object('session_id',convert_from(session_id,'UTF8'),'target',payload->'Value'->'Target','retired',payload->'Value'->'Retired','command',payload->'Value'->'Command','command_id',payload->'Value'->'CommandID','finished',payload->'Value'->'Finished') FROM work_sessions ORDER BY session_id LIMIT 200`)
		e.snapshotRuns()
	})
}

func (e *environment) snapshot(pool *pgxpool.Pool, label, query string) {
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, query)
	if err != nil {
		code := ""
		if pg, ok := err.(interface{ SQLState() string }); ok {
			code = pg.SQLState()
		}
		e.t.Logf("day snapshot %s query_failed=%T sqlstate=%s", label, err, code)
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			e.t.Logf("day snapshot %s scan_failed=%T", label, err)
			return
		}
		e.t.Logf("day snapshot %s=%s", label, raw)
		count++
	}
	e.t.Logf("day snapshot %s rows=%d read_error=%t", label, count, rows.Err() != nil)
}

func (e *environment) snapshotRuns() {
	if e.appPool == nil || e.core == nil {
		return
	}
	seen := map[string]bool{}
	collect := func(pool *pgxpool.Pool, query string) {
		if pool == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		rows, err := pool.Query(ctx, query)
		if err != nil {
			e.t.Logf("day diagnostic session_discovery_failed=%T", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var sid string
			if err := rows.Scan(&sid); err != nil {
				e.t.Logf("day diagnostic session_discovery_scan_failed=%T", err)
				return
			}
			if sid != "" {
				seen[sid] = true
			}
		}
		if err := rows.Err(); err != nil {
			e.t.Logf("day diagnostic session_discovery_incomplete=%T", err)
		}
	}
	collect(e.appPool, `SELECT DISTINCT session_id FROM orchestration_tasks WHERE session_id<>'' ORDER BY session_id LIMIT 200`)
	collect(e.appPool, `SELECT DISTINCT payload->>'Session' FROM session_creations WHERE coalesce(payload->>'Session','')<>'' LIMIT 200`)
	// no_progress 会清空任务的当前 Session；必须沿 Persona 的历史身份找回被退役的 Run。
	collect(e.personaPool, `SELECT convert_from(session_id,'UTF8') FROM work_sessions ORDER BY session_id LIMIT 200`)
	collect(e.personaPool, `SELECT DISTINCT sid FROM business_days CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(payload->'Sessions')='array' THEN payload->'Sessions' ELSE '[]'::jsonb END) AS s(sid) ORDER BY sid LIMIT 200`)
	var sessions []string
	for sid := range seen {
		sessions = append(sessions, sid)
	}
	sort.Strings(sessions)
	for _, sid := range sessions {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		events, err := eventlog.ReadAll(ctx, e.core.Stores.Log, sid, 0)
		cancel()
		if err != nil {
			e.t.Logf("day diagnostic session=%s event_read_failed=%T", sid, err)
			continue
		}
		e.t.Logf("day kernel_events BEGIN session=%s events=%d", sid, len(events))
		calls := map[string]string{}
		for _, event := range events {
			message := event.ProtoReflect()
			field := message.WhichOneof(message.Descriptor().Oneofs().ByName("payload"))
			kind, run, callID := "unknown", "", ""
			attempt := uint64(0)
			if field != nil {
				kind = string(field.Name())
				payload := message.Get(field).Message()
				if id := payload.Descriptor().Fields().ByName("run_id"); id != nil {
					run = payload.Get(id).String()
				}
				if id := payload.Descriptor().Fields().ByName("call_id"); id != nil {
					callID = payload.Get(id).String()
				}
				if field := payload.Descriptor().Fields().ByName("attempt"); field != nil {
					attempt = payload.Get(field).Uint()
				}
			}
			e.t.Logf("day kernel_event session=%s seq=%d kind=%s run=%s attempt=%d call=%s", sid, event.GetSeq(), kind, run, attempt, callID)
			if started := event.GetToolCallStarted(); started != nil {
				e.t.Logf("day kernel_call_started session=%s run=%s call=%s read_only=%t", sid, run, callID, started.GetReadOnly())
			}
			for _, call := range event.GetAssistantMessage().GetToolCalls() {
				calls[call.GetCallId()] = call.GetCapability()
				e.t.Logf("day kernel_tool session=%s run=%s call=%s tool=%s", sid, run, call.GetCallId(), e.redact(call.GetCapability()))
			}
			if result := event.GetToolResult(); result != nil {
				preview := ""
				if !e.ark {
					text := model.Text(result.GetContent())
					if result.GetIsError() || strings.Contains(text, "没有被接受") || strings.Contains(text, "没有收下") {
						preview = e.preview(text)
					}
				}
				e.t.Logf("day kernel_result session=%s run=%s call=%s tool=%s is_error=%t error_preview=%q", sid, run, result.GetCallId(), e.redact(calls[result.GetCallId()]), result.GetIsError(), preview)
			}
			if failed := event.GetRunFailed(); failed != nil {
				preview := ""
				if !e.ark {
					preview = e.preview(failed.GetReason())
				}
				e.t.Logf("day kernel_failed session=%s run=%s category=%s error_preview=%q", sid, run, failureCategory(failed.GetReason()), preview)
			}
		}
		e.t.Logf("day kernel_events END session=%s", sid)
	}
}

func (e *environment) preview(text string) string {
	chars := []rune(e.redact(text))
	if len(chars) > 200 {
		return string(chars[:200]) + "…"
	}
	return string(chars)
}

type scriptObservation struct {
	Model  string       `json:"model"`
	Tools  []string     `json:"available_tools"`
	Calls  []scriptCall `json:"calls"`
	Finish string       `json:"finish_reason"`
	Error  string       `json:"error,omitempty"`
}
type scriptCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type scriptTrace struct {
	mu      sync.Mutex
	entries []scriptObservation
}

func (s *scriptTrace) record(req *model.Request, response *model.Response, err error) {
	item := scriptObservation{Model: req.Model, Tools: []string{}, Calls: []scriptCall{}}
	for _, tool := range req.Tools {
		item.Tools = append(item.Tools, tool.Name)
	}
	sort.Strings(item.Tools)
	if response != nil {
		item.Finish = response.FinishReason
		for _, call := range response.ToolCalls {
			item.Calls = append(item.Calls, scriptCall{call.GetCapability(), call.GetArgumentsJson()})
		}
	}
	if err != nil {
		item.Error = err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, item)
	if len(s.entries) > 200 {
		s.entries = s.entries[len(s.entries)-200:]
	}
}

func (e *environment) dumpScript() {
	if e.script == nil || e.ark {
		return
	}
	e.script.mu.Lock()
	entries := append([]scriptObservation(nil), e.script.entries...)
	e.script.mu.Unlock()
	e.t.Logf("day script BEGIN responses=%d synthetic=true", len(entries))
	for i, item := range entries {
		item.Error = e.preview(item.Error)
		raw, err := json.Marshal(item)
		if err == nil {
			e.t.Logf("day script response=%d %s", i+1, e.redact(string(raw)))
		}
	}
	e.t.Log("day script END")
}

// 底层错误后缀可能含提供商内容；只把已知前缀映射为类别，绝不打印 reason 原值。
func failureCategory(reason string) string {
	for _, item := range []struct{ reason, category string }{
		{"day script: no attested captures", "script_no_attested_captures"},
		{"day script: unusable capture page", "script_unusable_capture_page"},
		{"day script: unrecognized capture page", "script_unrecognized_capture_page"},
		{"day script: tool failed", "script_tool_failed"},
		{"day script: changes rejected", "script_changes_rejected"},
		{"day script: missing page cursor", "script_missing_page_cursor"},
		{"day script: missing continuation", "script_missing_continuation"},
		{"day script: unexpected tool result", "script_unexpected_tool_result"},
		{"day script: related result lacks commitment/project identity", "script_missing_related_identity"},
	} {
		if reason == "model error: "+item.reason {
			return item.category
		}
	}
	for _, item := range []struct{ prefix, category string }{
		{"model error:", "model_error"}, {"summary error:", "summary_error"}, {"internal error:", "internal_error"},
	} {
		if strings.HasPrefix(reason, item.prefix) {
			return item.category
		}
	}
	return "other"
}

func TestDiagnosticTailAndRedaction(t *testing.T) {
	tail := &lineTail{}
	for i := 0; i < 205; i++ {
		_, _ = tail.Write([]byte("old\n"))
	}
	_, _ = tail.Write([]byte("last"))
	lines := tail.tail()
	if len(lines) != 200 || lines[len(lines)-1] != "last" {
		t.Fatal("stderr tail lost its bound or partial line")
	}
	e := &environment{environ: []string{"ARK_API_KEY=private-model-key"}, token: "private-access-token", control: &control{token: "private-controller-token"}}
	value := e.redact(`id=run_1 Bearer opaque-credential password="database-password" postgres://user:pass@localhost/db private-model-key private-access-token private-controller-token`)
	for _, secret := range []string{"opaque-credential", "database-password", "user:pass", "private-model-key", "private-access-token", "private-controller-token"} {
		if strings.Contains(value, secret) {
			t.Fatal("diagnostics retained a credential")
		}
	}
	if !strings.Contains(value, "id=run_1") {
		t.Fatal("diagnostics lost the execution ID")
	}
	if failureCategory("model error: private-provider-content") != "model_error" {
		t.Fatal("unsafe failure classification")
	}
}

func TestDiagnosticHTTPTracePreservesResponseAndFlush(t *testing.T) {
	trace := &httpTrace{}
	response := httptest.NewRecorder()
	trace.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("private-response"))
	})).ServeHTTP(response, httptest.NewRequest("POST", "/v1/sessions?private=query", strings.NewReader("private-input")))
	if response.Code != http.StatusCreated || response.Body.String() != "private-response" || !response.Flushed {
		t.Fatal("HTTP diagnostics changed the kernel response")
	}
	if len(trace.counts) != 1 || trace.counts[httpObservation{"POST", "/v1/sessions", http.StatusCreated}] != 1 {
		t.Fatal("HTTP diagnostics lost status or retained query")
	}
	for i := 0; i < 250; i++ {
		trace.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/outbox?after=private-cursor", nil))
	}
	if len(trace.counts) != 2 || trace.counts[httpObservation{"GET", "/v1/outbox", http.StatusOK}] != 250 || trace.counts[httpObservation{"POST", "/v1/sessions", http.StatusCreated}] != 1 {
		t.Fatal("outbox polling displaced the Session creation evidence")
	}
}
