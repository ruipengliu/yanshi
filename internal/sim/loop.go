package sim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/extloop"
	"yanshi/internal/feed"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
)

// 外部循环（ADR-0062）：Options.ExternalLoop 让 sim 的两个 AgentDef 版本由业务线 bl 登记的外部循环 sim 驱动。
// 请求经真实的 extloop.Client 发出（protojson、NDJSON 与各项上限），由进程内的 loopTransport 在同一个 goroutine 中
// 同步应答：没有网络，种子照样可复现。回答的内容仍由 simModel 决定，沿用它的错误、崩溃与工具偏向；循环自己的故障
// 用单独的随机流 loopRNG 抽样：超时、5xx、不合法的回答、收到回答后写入日志的途中崩溃、旧 Attempt 的回答迟到。
//
// 外部服务按契约只靠请求工作：状态编码为"Run ID#第几步#填充"，下一步由请求带回的状态推出。日志中第 k 条回答的状态
// 必须是第 k 步，于是状态链断开、跨 Run 串用、不合法或迟到的回答被写入，都在 checkLoopLog 中现形。
const (
	simLoopServer = "sim"
	simLoopModel  = "loop/" + simLoopServer
	// simLoopMissingTool 是不合法回答中调用的、请求的 tools 里没有的工具。
	simLoopMissingTool = "sim_not_a_tool"
	// simLoopInvalidState 是不合法回答带的状态：它一旦写入日志，状态链检查即失败。
	simLoopInvalidState = "invalid"
)

var (
	loopMarshal   = protojson.MarshalOptions{}
	loopUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
	// errLoopTimeout 是注入的超时或连接失败：extloop.Client 把它报告为请求失败。
	errLoopTimeout = errors.New("simulated loop timeout")
)

func (w *World) loopRegistry() *extloop.Registry {
	return extloop.NewRegistry([]extloop.Server{{
		Name: simLoopServer, BusinessLine: "bl", URL: "http://loop.sim/step", Timeout: extloop.DefaultTimeout,
	}}, &http.Client{Transport: loopTransport{w}})
}

// loopTransport 把 extloop.Client 的请求交给 World.serveLoop。http.Client 在调用方的 goroutine 中调用 RoundTrip
// （没有设置 Client.Timeout），应答因此与 Worker.Step 同步。
type loopTransport struct{ w *World }

func (t loopTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.w.serveLoop(req)
}

// serveLoop 是脚本化的外部循环服务。注入的失败置 w.loopFault，stepWorker 据此把 Worker 报告的这一步错误当作预期。
func (w *World) serveLoop(hr *http.Request) (*http.Response, error) {
	ctx := hr.Context()
	b, err := io.ReadAll(hr.Body)
	_ = hr.Body.Close()
	if err != nil {
		return nil, err
	}
	var req v1.LoopStepRequest
	if err := loopUnmarshal.Unmarshal(b, &req); err != nil {
		w.violations = append(w.violations, "loop request is not valid protojson: "+err.Error())
		return nil, err
	}
	w.Stats.LoopSteps++
	after := w.checkLoopRequest(ctx, &req)
	// 同一步（Run、步序、带回的状态）再次被请求：崩溃、错误、不合法的回答或内容安全等待之后的重答（ADR-0062 第 3 条）。
	key := fmt.Sprintf("%s/%d/%s", req.GetRunId(), req.GetTurn(), req.GetStateVersion())
	if w.loopSeen[key]++; w.loopSeen[key] > 1 {
		w.Stats.LoopRepeats++
	}
	if w.faults {
		switch x := w.loopRNG.Float64(); {
		case x < 0.03:
			w.Stats.LoopTimeouts++
			w.loopFault = true
			return nil, errLoopTimeout
		case x < 0.05:
			w.Stats.LoopServerErrors++
			w.loopFault = true
			return loopResponse(hr, http.StatusBadGateway, "text/plain", []byte("bad gateway")), nil
		}
	}
	resp, err := (&simModel{w: w}).Generate(ctx, loopModelRequest(&req), nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // 模型脚本在这一步中途让 Worker 崩溃
		}
		// 外部服务内部的模型出错（含超长：契约中没有"上下文超长"，平台不据此压缩）。
		w.loopFault = true
		return loopResponse(hr, http.StatusInternalServerError, "text/plain", []byte(err.Error())), nil
	}
	k := 1
	if _, prev, ok := parseLoopState(req.GetState()); ok {
		k = prev + 1
	}
	out := &v1.LoopStepResponse{
		Content: resp.Content, ToolCalls: resp.ToolCalls,
		State: loopState(req.GetRunId(), k, w.loopRNG.IntN(512)), StateVersion: fmt.Sprintf("v%d", k),
		// 自报的用量平台暂不记录（ADR-0062）；照样发送，检验它不进入用量与日志。
		ReportedUsage: []*v1.LoopReportedUsage{{Model: "sim/inner", Usage: resp.Usage}},
	}
	if w.faults && w.loopRNG.Float64() < 0.05 {
		w.Stats.LoopInvalidReplies++
		w.loopFault = true
		out.State, out.StateVersion = []byte(simLoopInvalidState), simLoopInvalidState
		switch w.loopRNG.IntN(5) {
		case 0:
			out.ToolCalls = append(out.ToolCalls, &v1.ToolCall{Capability: simLoopMissingTool, ArgumentsJson: "{}"})
		case 1:
			out.Content = append(out.Content, &v1.ContentBlock{Kind: &v1.ContentBlock_Card{Card: &v1.Card{CardId: "c0", Title: "卡片"}}})
		case 2:
			out.State = bytes.Repeat([]byte("s"), session.MaxLoopState+1)
		case 3:
			// 流式回答缺少最后一行（ADR-0056 Q7）。
			return loopResponse(hr, http.StatusOK, "application/x-ndjson", []byte(`{"delta":{"text":"半句"}}`+"\n")), nil
		case 4:
			return loopResponse(hr, http.StatusOK, "application/json", []byte(`{"content":"不是内容块"}`)), nil
		}
	} else if w.faults && w.loopDepth == 0 && w.loopRNG.Float64() < 0.04 && w.lateTakeover(ctx, &req, after) {
		// 回答迟到：这一步的 Attempt 已被取代，Worker 不得写入它（ADR-0062 第 3 条）。日志中若出现它，
		// 同一步就有两条回答，状态链检查失败。
		w.Stats.LoopLateReplies++
	} else if w.faults && w.loopRNG.Float64() < 0.03 {
		// 回答已发出，Worker 在校验、计量、送检与写日志的途中崩溃：取决于崩溃的时刻，回答可能已写入，也可能没有，
		// 没写入时同一步以新的 Attempt 重答。
		w.Stats.LoopCrashesAfterAnswer++
		w.crash()
	}
	if w.loopRNG.IntN(2) == 0 {
		w.Stats.LoopStreamed++
		var body []byte
		if text := model.Text(out.Content); text != "" {
			body = append(loopLine(&v1.LoopStreamLine{Line: &v1.LoopStreamLine_Delta{Delta: &v1.LoopDelta{Text: text}}}), '\n')
		}
		body = append(body, loopLine(&v1.LoopStreamLine{Line: &v1.LoopStreamLine_Response{Response: out}})...)
		return loopResponse(hr, http.StatusOK, "application/x-ndjson; charset=utf-8", append(body, '\n')), nil
	}
	return loopResponse(hr, http.StatusOK, "application/json", loopLine(out)), nil
}

func loopLine(m proto.Message) []byte {
	b, err := loopMarshal.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func loopResponse(req *http.Request, status int, contentType string, body []byte) *http.Response {
	return &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Request: req,
	}
}

// loopModelRequest 把外部服务收到的请求还原为 simModel 的输入：外部服务看到的就是这些。
func loopModelRequest(req *v1.LoopStepRequest) *model.Request {
	out := &model.Request{System: req.GetSystem()}
	for _, m := range req.GetMessages() {
		out.Messages = append(out.Messages, model.Message{
			Role: model.Role(m.GetRole()), Content: m.GetContent(), ToolCalls: m.GetToolCalls(), ToolCallID: m.GetToolCallId(),
		})
	}
	for _, t := range req.GetTools() {
		out.Tools = append(out.Tools, model.ToolSpec{Name: t.GetName(), Description: t.GetDescription(), InputSchema: []byte(t.GetInputSchemaJson())})
	}
	return out
}

func loopState(runID string, k, pad int) []byte {
	return fmt.Appendf(nil, "%s#%d#%s", runID, k, strings.Repeat("s", pad))
}

func parseLoopState(b []byte) (runID string, k int, ok bool) {
	parts := strings.SplitN(string(b), "#", 3)
	if len(parts) != 3 {
		return "", 0, false
	}
	k, err := strconv.Atoi(parts[1])
	if err != nil || k < 1 {
		return "", 0, false
	}
	return parts[0], k, true
}

// checkLoopRequest 对照日志检查一步请求，返回日志当前的末尾序号。来自当前 Attempt 的请求：
//   - 状态与版本是本 Run 最近一条写入日志的回答带的（第一步为空），步序是已写入的回答数加一。由此，已写入日志的步
//     不会在接管或恢复时被再次请求：Worker 从日志回放出状态，而不是重放外部服务（ADR-0062 第 2 条）；
//   - "上一步"与 messages 中最近一条回答的调用按位置一一对应，每个结果都在 messages 中、内容与其工具消息相同：
//     压缩没有越过最近一步（ADR-0062 第 5 条）。
//
// 来自已被取代的 Attempt 或已删除 Session 的请求（它的回答不会写入，契约允许）只计数。
func (w *World) checkLoopRequest(ctx context.Context, req *v1.LoopStepRequest) uint64 {
	fail := func(format string, args ...any) {
		w.violations = append(w.violations, fmt.Sprintf("loop request %s turn %d: ", req.GetRunId(), req.GetTurn())+fmt.Sprintf(format, args...))
	}
	// Worker 每步请求之前查删除记录（ADR-0062 第 7 条）；删除记录先于清理写入，且模拟中查与请求在同一步里，
	// 之间没有别的写入，所以请求时 Session 不应已有删除记录。查删除记录的存储故障不作判断。
	if gone, err := lifecycle.Deleted(ctx, w.deletions, req.GetSessionId()); err == nil && gone {
		fail("requested after the session was deleted")
		return 0
	}
	events, err := eventlog.ReadAll(ctx, w.log, req.GetSessionId(), 0)
	if err != nil {
		fail("read log: %v", err)
		return 0
	}
	st, err := session.Reduce(events)
	if err != nil {
		fail("reduce: %v", err)
		return 0
	}
	if len(events) == 0 {
		fail("requested for a session whose log is gone")
		return 0
	}
	r := st.Run(req.GetRunId())
	if r == nil || r.Status != session.RunRunning || r.Attempt != req.GetAttempt() {
		w.Stats.LoopStaleRequests++
		return st.Seq
	}
	if !bytes.Equal(req.GetState(), r.LoopState) || req.GetStateVersion() != r.LoopStateVersion {
		fail("state %q/%s, latest logged reply has %q/%s", req.GetState(), req.GetStateVersion(), r.LoopState, r.LoopStateVersion)
	}
	if int(req.GetTurn()) != r.Turns+1 {
		fail("run has %d logged replies", r.Turns)
	}
	results := req.GetPrevious().GetResults()
	if len(req.GetState()) == 0 {
		if len(results) > 0 {
			fail("first step carries %d previous results", len(results))
		}
		return st.Seq
	}
	if run, _, ok := parseLoopState(req.GetState()); !ok || run != req.GetRunId() {
		fail("state belongs to run %q", run)
	}
	msgs := req.GetMessages()
	last := -1
	for i, m := range msgs {
		if m.GetRole() == string(model.RoleAssistant) {
			last = i
		}
	}
	if last < 0 {
		fail("latest step missing from messages")
		return st.Seq
	}
	calls := msgs[last].GetToolCalls()
	if len(calls) != len(results) {
		fail("latest step has %d calls, previous has %d results", len(calls), len(results))
		return st.Seq
	}
	toolMsgs := map[string]*v1.LoopMessage{}
	for _, m := range msgs[last+1:] {
		if m.GetRole() == string(model.RoleTool) {
			toolMsgs[m.GetToolCallId()] = m
		}
	}
	for i, res := range results {
		if res.GetIndex() != uint32(i) || res.GetCallId() != calls[i].GetCallId() || res.GetCapability() != calls[i].GetCapability() {
			fail("previous result %d (%s) does not map to call %s", i, res.GetCallId(), calls[i].GetCallId())
		}
		m := toolMsgs[res.GetCallId()]
		if m == nil {
			fail("result of %s missing from messages", res.GetCallId())
			continue
		}
		// 工具消息中的错误结果多一行前缀（runtime.toolMessage），"上一步"改以 is_error 标记。
		want := res.GetContent()
		if res.GetIsError() {
			want = append(model.TextBlocks("[error] "), want...)
		}
		if !blocksEqual(m.GetContent(), want) {
			fail("result of %s differs from its tool message", res.GetCallId())
		}
	}
	return st.Seq
}

func blocksEqual(a, b []*v1.ContentBlock) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// lateTakeover 让这一步的租约在请求途中过期，并步进其余 Worker，直到其中之一接管这个 Run（新的 Attempt）或中断它。
// 接管发生时等 Worker 的监视取消这一步再返回：回答因此总在 Worker.during 处被丢弃，不会时而在那里、时而在写日志时
// 被拒绝；后者还会计量这一步，使用量与之后的配额判断随 goroutine 调度变化，种子不再可复现。
func (w *World) lateTakeover(ctx context.Context, req *v1.LoopStepRequest, after uint64) bool {
	w.loopDepth++
	defer func() { w.loopDepth-- }()
	w.clock.Advance(leaseTTL + time.Second)
	w.tracef("loop %s: lease expires mid-step", req.GetRunId())
	for range 2 {
		for j := range w.workers {
			if j == w.current {
				continue
			}
			if err := w.nestedStep(j); err != nil {
				w.violations = append(w.violations, "step during late loop reply: "+err.Error())
				return false
			}
			if w.supersededSince(req, after) {
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					w.violations = append(w.violations, "superseded loop step was not cancelled")
				}
				return true
			}
		}
	}
	return false
}

// supersededSince 报告 seq 之后的日志中是否有令 Worker 取消这一步的事件（runtime.watchSupersede）。
func (w *World) supersededSince(req *v1.LoopStepRequest, seq uint64) bool {
	events, err := eventlog.ReadAll(context.Background(), w.log, req.GetSessionId(), seq)
	if err != nil {
		return false
	}
	for _, e := range events {
		if e.GetAttemptStarted().GetRunId() == req.GetRunId() || e.GetRunInterrupted().GetRunId() == req.GetRunId() {
			return true
		}
	}
	return false
}

// nestedStep 在一步之中步进另一个 Worker，并恢复外层这一步的崩溃函数与记账。
func (w *World) nestedStep(j int) error {
	crash, cur, ro, fault := w.crash, w.current, w.readOnlyWorker, w.loopFault
	defer func() { w.crash, w.current, w.readOnlyWorker, w.loopFault = crash, cur, ro, fault }()
	return w.stepWorker(j)
}

// checkLoopLog 检查外部循环驱动的日志：
//   - 每个 Run 中第 k 条回答带第 k 步的状态与版本，且属于本 Run：状态链连续、不跨 Run，不合法与迟到的回答没有写入；
//   - 被内容安全拦截的回答不带状态，且是该 Run 的最后一条回答（ADR-0062 第 5 条）；
//   - 对外事件流中没有状态（feed.Public，ADR-0056 第 7 条）。
func checkLoopLog(sid string, events []*v1.Event) error {
	turns, blocked, ended := map[string]int{}, map[string]bool{}, map[string]bool{}
	for _, e := range events {
		if pm := feed.Public(e).GetAssistantMessage(); len(pm.GetLoopState()) > 0 || pm.GetLoopStateVersion() != "" {
			return fmt.Errorf("invariant: %s: loop state of seq %d in the public feed", sid, e.GetSeq())
		}
		if cm := e.GetContentModerated(); cm != nil && cm.GetStage() == string(moderation.Output) {
			blocked[cm.GetRunId()] = true
			continue
		}
		m := e.GetAssistantMessage()
		if m == nil {
			continue
		}
		run := m.GetRunId()
		if m.GetModel() != simLoopModel {
			return fmt.Errorf("invariant: %s: reply at %d recorded as %q", sid, e.GetSeq(), m.GetModel())
		}
		if ended[run] {
			return fmt.Errorf("invariant: %s: run %s has a reply at %d after a blocked one", sid, run, e.GetSeq())
		}
		if blocked[run] {
			if len(m.GetLoopState()) > 0 || m.GetLoopStateVersion() != "" || len(m.GetToolCalls()) > 0 {
				return fmt.Errorf("invariant: %s: blocked reply at %d kept loop state or calls", sid, e.GetSeq())
			}
			ended[run] = true
			continue
		}
		turns[run]++
		got, k, ok := parseLoopState(m.GetLoopState())
		if !ok || got != run || k != turns[run] || m.GetLoopStateVersion() != fmt.Sprintf("v%d", k) {
			return fmt.Errorf("invariant: %s: reply %d of run %s at seq %d carries state %.40q/%s",
				sid, turns[run], run, e.GetSeq(), m.GetLoopState(), m.GetLoopStateVersion())
		}
	}
	return nil
}
