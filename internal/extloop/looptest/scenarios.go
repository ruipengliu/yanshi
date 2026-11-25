package looptest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

// allScenarios 定义固定名称与顺序的一致性场景。
var allScenarios = []scenario{
	{name: "single-step", run: runSingleStep},
	{name: "parallel-calls", run: runParallelCalls},
	{name: "rejected-and-failed", run: runRejectedAndFailed},
	{name: "interjection", run: runInterjection},
	{name: "repeat-step", run: runRepeatStep},
	{name: "repeat-with-new-input", run: runRepeatWithNewInput},
	{name: "new-run-empty-state", run: runNewRunEmptyState},
	{name: "compaction", run: runCompaction},
	{name: "state-limit", run: runStateLimit},
	{name: "restart", run: runRestart},
	{name: "model-failure", run: runModelFailure},
	{name: "streaming", run: runStreaming},
}

func standardTools() []*v1.LoopTool {
	// 工具按名称排序，与平台组装请求的约定一致（loop.proto）。
	return []*v1.LoopTool{
		{
			Name:            "lookup",
			Description:     "按关键字查询信息",
			InputSchemaJson: `{"type":"object","properties":{"key":{"type":"string"},"idx":{"type":"integer"}},"required":["key"]}`,
		},
		{
			Name:            "write_note",
			Description:     "记录一条笔记",
			InputSchemaJson: `{"type":"object","properties":{"text":{"type":"string"},"idx":{"type":"integer"}},"required":["text"]}`,
		},
	}
}

func systemPrompt(scenarioName string) string {
	return "你是 yanshi 外部循环一致性测试助手。\n" + markerPrefix + scenarioName
}

func baseRequest(sid, rid, scenarioName string, attempt, turn uint32, msgs []*v1.LoopMessage) *v1.LoopStepRequest {
	return &v1.LoopStepRequest{
		SessionId: sid,
		RunId:     rid,
		Attempt:   attempt,
		Turn:      turn,
		EndUser:   "lt_user",
		Agent:     &v1.AgentRef{Name: "looptest", Version: "1"},
		System:    systemPrompt(scenarioName),
		Messages:  msgs,
		Tools:     standardTools(),
	}
}

func userMsg(text string) *v1.LoopMessage {
	return &v1.LoopMessage{Role: string(model.RoleUser), Content: model.TextBlocks(text)}
}

func assistantMsg(content []*v1.ContentBlock, calls []*v1.ToolCall) *v1.LoopMessage {
	return &v1.LoopMessage{Role: string(model.RoleAssistant), Content: content, ToolCalls: calls}
}

// toolResultMsg 与平台 runtime.toolMessage 一致：is_error 时在消息文本前加 "[error] " 前缀，
// 而 previous.results 中以 is_error 布尔字段表达、不加前缀。
func toolResultMsg(callID, text string, isError bool) *v1.LoopMessage {
	blocks := model.TextBlocks(text)
	if isError {
		blocks = append(model.TextBlocks("[error] "), blocks...)
	}
	return &v1.LoopMessage{Role: string(model.RoleTool), ToolCallId: callID, Content: blocks}
}

func rewriteCalls(calls []*v1.ToolCall, prefix string) []*v1.ToolCall {
	out := make([]*v1.ToolCall, len(calls))
	for i, tc := range calls {
		out[i] = &v1.ToolCall{
			CallId:        fmt.Sprintf("%s_%d", prefix, i),
			Capability:    tc.GetCapability(),
			ArgumentsJson: tc.GetArgumentsJson(),
		}
	}
	return out
}

func jsonEqual(a, b string) bool {
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func countToolMsgs(msgs []ModelMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == string(model.RoleTool) {
			n++
		}
	}
	return n
}

func hasMessageContaining(msgs []ModelMessage, substr string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

// checkToolPairing 检查模型看到的最近一条带 tool_calls 的助手消息与其后的工具消息：
// 每条工具消息都与助手消息的调用一一配对，且第 i 个调用配的是 wantSubstrs[i]。
func checkToolPairing(msgs []ModelMessage, wantSubstrs []string) error {
	lastAsst := -1
	for i, m := range msgs {
		if m.Role == string(model.RoleAssistant) && len(m.ToolCalls) > 0 {
			lastAsst = i
		}
	}
	if lastAsst < 0 {
		return fmt.Errorf("model did not see an assistant message with tool_calls")
	}
	calls := msgs[lastAsst].ToolCalls
	if len(calls) != len(wantSubstrs) {
		return fmt.Errorf("assistant message has %d tool_calls, want %d", len(calls), len(wantSubstrs))
	}
	toolMsgs := map[string]string{}
	toolCount := 0
	for _, m := range msgs[lastAsst+1:] {
		if m.Role != string(model.RoleTool) {
			continue
		}
		toolCount++
		if m.ToolCallID == "" {
			return fmt.Errorf("tool message missing tool_call_id")
		}
		toolMsgs[m.ToolCallID] = m.Content
	}
	if toolCount != len(wantSubstrs) {
		return fmt.Errorf("model saw %d tool messages after assistant, want %d", toolCount, len(wantSubstrs))
	}
	for i, tc := range calls {
		if tc.ID == "" {
			return fmt.Errorf("assistant tool_calls[%d] missing id", i)
		}
		got, ok := toolMsgs[tc.ID]
		if !ok {
			return fmt.Errorf("no tool message paired with tool_calls[%d] (id=%q)", i, tc.ID)
		}
		if !strings.Contains(got, wantSubstrs[i]) {
			return fmt.Errorf("tool_calls[%d] (id=%q) paired with wrong result: want substring %q", i, tc.ID, wantSubstrs[i])
		}
	}
	return nil
}

// 1. single-step：单步纯文本回答，核对系统标记与全部工具名。
func runSingleStep(ctx context.Context, r *runner) (Status, string) {
	const name = "single-step"
	r.cfg.Model.beginScenario(name, func(ModelCall) ModelReply {
		return ModelReply{Text: "LOOPTEST_SINGLE_OK"}
	})
	defer r.cfg.Model.endScenario(name)

	req := baseRequest(r.nextID("s", name), r.nextID("r", name), name, 1, 1, []*v1.LoopMessage{userMsg("请回答准备就绪。")})
	out, err := r.sendStep(ctx, req)
	if err != nil {
		return StatusFail, err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out.resp.GetToolCalls()) != 0 {
		return StatusFail, fmt.Sprintf("expected 0 tool_calls, got %d", len(out.resp.GetToolCalls()))
	}
	if !strings.Contains(model.Text(out.resp.GetContent()), "LOOPTEST_SINGLE_OK") {
		return StatusFail, fmt.Sprintf("reply %q does not contain expected text", model.Text(out.resp.GetContent()))
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) == 0 {
		return StatusFail, "model was not called"
	}
	seenTools := map[string]bool{}
	for _, t := range calls[0].Tools {
		seenTools[t.Name] = true
	}
	for _, want := range req.GetTools() {
		if !seenTools[want.GetName()] {
			return StatusFail, fmt.Sprintf("model did not receive tool %q", want.GetName())
		}
	}
	return StatusPass, ""
}

// 2. parallel-calls：一步 3 个并行调用、平台改写调用 ID 后按位置返回结果（含一条 is_error）。
func runParallelCalls(ctx context.Context, r *runner) (Status, string) {
	const name = "parallel-calls"
	wantCalls := []ModelToolCall{
		{ID: "fm_par_1", Name: "lookup", Arguments: `{"idx":0,"key":"k0"}`},
		{ID: "fm_par_2", Name: "lookup", Arguments: `{"idx":1,"key":"k1"}`},
		{ID: "fm_par_3", Name: "write_note", Arguments: `{"idx":2,"text":"n2"}`},
	}
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		if countToolMsgs(call.Messages) == 0 {
			return ModelReply{ToolCalls: wantCalls}
		}
		return ModelReply{Text: "LOOPTEST_PARALLEL_DONE"}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	u := userMsg("请并行调用三个工具。")
	req1 := baseRequest(sid, rid, name, 1, 1, []*v1.LoopMessage{u})
	out1, err := r.sendStep(ctx, req1)
	if err != nil {
		return StatusFail, "step 1: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	gotCalls := out1.resp.GetToolCalls()
	if len(gotCalls) != len(wantCalls) {
		return StatusFail, fmt.Sprintf("step 1: got %d tool_calls, want %d", len(gotCalls), len(wantCalls))
	}
	for i, want := range wantCalls {
		got := gotCalls[i]
		if got.GetCapability() != want.Name {
			return StatusFail, fmt.Sprintf("step 1: tool_calls[%d] capability %q, want %q", i, got.GetCapability(), want.Name)
		}
		if !jsonEqual(got.GetArgumentsJson(), want.Arguments) {
			return StatusFail, fmt.Sprintf("step 1: tool_calls[%d] arguments %s, want %s", i, got.GetArgumentsJson(), want.Arguments)
		}
	}

	// 平台改写调用 ID（与模型给出的 fm_par_* 不同），按 index 给出结果，其中第 2 条为 is_error。
	rewritten := rewriteCalls(gotCalls, "plat_par")
	resTokens := []string{"PAR_RESULT_TOKEN_0", "PAR_ERROR_TOKEN_1", "PAR_RESULT_TOKEN_2"}
	isErrs := []bool{false, true, false}
	msgs2 := []*v1.LoopMessage{u, assistantMsg(out1.resp.GetContent(), rewritten)}
	prev := &v1.LoopPreviousStep{}
	for i, tc := range rewritten {
		msgs2 = append(msgs2, toolResultMsg(tc.GetCallId(), resTokens[i], isErrs[i]))
		prev.Results = append(prev.Results, &v1.LoopCallResult{
			Index:      uint32(i),
			CallId:     tc.GetCallId(),
			Capability: tc.GetCapability(),
			Content:    model.TextBlocks(resTokens[i]),
			IsError:    isErrs[i],
		})
	}
	req2 := baseRequest(sid, rid, name, 1, 2, msgs2)
	req2.State = out1.resp.GetState()
	req2.StateVersion = out1.resp.GetStateVersion()
	req2.Previous = prev

	before := len(r.cfg.Model.Calls(name))
	out2, err := r.sendStep(ctx, req2)
	if err != nil {
		return StatusFail, "step 2: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out2.resp.GetToolCalls()) != 0 {
		return StatusFail, fmt.Sprintf("step 2: expected 0 tool_calls, got %d", len(out2.resp.GetToolCalls()))
	}
	if !strings.Contains(model.Text(out2.resp.GetContent()), "LOOPTEST_PARALLEL_DONE") {
		return StatusFail, fmt.Sprintf("step 2: reply %q missing expected text", model.Text(out2.resp.GetContent()))
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) <= before {
		return StatusFail, "step 2: model was not called"
	}
	if err := checkToolPairing(calls[len(calls)-1].Messages, resTokens); err != nil {
		return StatusFail, "step 2: " + err.Error()
	}
	return StatusPass, ""
}

// 3. rejected-and-failed：审批被拒与执行失败的结果以 is_error 标记返回，模型看到两条说明并正常完成。
func runRejectedAndFailed(ctx context.Context, r *runner) (Status, string) {
	const name = "rejected-and-failed"
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		if countToolMsgs(call.Messages) == 0 {
			return ModelReply{ToolCalls: []ModelToolCall{
				{ID: "fm_rf_1", Name: "write_note", Arguments: `{"text":"sensitive"}`},
				{ID: "fm_rf_2", Name: "lookup", Arguments: `{"key":"broken"}`},
			}}
		}
		return ModelReply{Text: "LOOPTEST_RF_DONE"}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	u := userMsg("请写笔记并查询。")
	out1, err := r.sendStep(ctx, baseRequest(sid, rid, name, 1, 1, []*v1.LoopMessage{u}))
	if err != nil {
		return StatusFail, "step 1: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out1.resp.GetToolCalls()) != 2 {
		return StatusFail, fmt.Sprintf("step 1: got %d tool_calls, want 2", len(out1.resp.GetToolCalls()))
	}

	rewritten := rewriteCalls(out1.resp.GetToolCalls(), "plat_rf")
	// 措辞与平台 internal/runtime/worker.go 审批被拒段保持一致。
	errTexts := []string{
		"denied: the user rejected this call; it was not executed",
		"execution failed: upstream error (LT_EXEC_FAIL)",
	}
	msgs2 := []*v1.LoopMessage{u, assistantMsg(out1.resp.GetContent(), rewritten)}
	prev := &v1.LoopPreviousStep{}
	for i, tc := range rewritten {
		msgs2 = append(msgs2, toolResultMsg(tc.GetCallId(), errTexts[i], true))
		prev.Results = append(prev.Results, &v1.LoopCallResult{
			Index:      uint32(i),
			CallId:     tc.GetCallId(),
			Capability: tc.GetCapability(),
			Content:    model.TextBlocks(errTexts[i]),
			IsError:    true,
		})
	}
	req2 := baseRequest(sid, rid, name, 1, 2, msgs2)
	req2.State = out1.resp.GetState()
	req2.StateVersion = out1.resp.GetStateVersion()
	req2.Previous = prev

	before := len(r.cfg.Model.Calls(name))
	out2, err := r.sendStep(ctx, req2)
	if err != nil {
		return StatusFail, "step 2: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out2.resp.GetToolCalls()) != 0 || !strings.Contains(model.Text(out2.resp.GetContent()), "LOOPTEST_RF_DONE") {
		return StatusFail, fmt.Sprintf("step 2: unexpected reply %q (calls=%d)", model.Text(out2.resp.GetContent()), len(out2.resp.GetToolCalls()))
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) <= before {
		return StatusFail, "step 2: model was not called"
	}
	if err := checkToolPairing(calls[len(calls)-1].Messages, []string{"denied: the user rejected this call", "LT_EXEC_FAIL"}); err != nil {
		return StatusFail, "step 2: " + err.Error()
	}
	return StatusPass, ""
}

// 4. interjection：工具结果之后还有用户插话与后台结果文本，模型须看到结果与插话且顺序在结果之后。
func runInterjection(ctx context.Context, r *runner) (Status, string) {
	const name = "interjection"
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		if countToolMsgs(call.Messages) == 0 {
			return ModelReply{ToolCalls: []ModelToolCall{
				{ID: "fm_int_1", Name: "lookup", Arguments: `{"key":"initial"}`},
			}}
		}
		return ModelReply{Text: "LOOPTEST_INTERJECTION_DONE"}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	u := userMsg("开始执行。")
	out1, err := r.sendStep(ctx, baseRequest(sid, rid, name, 1, 1, []*v1.LoopMessage{u}))
	if err != nil {
		return StatusFail, "step 1: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out1.resp.GetToolCalls()) != 1 {
		return StatusFail, fmt.Sprintf("step 1: got %d tool_calls, want 1", len(out1.resp.GetToolCalls()))
	}

	rewritten := rewriteCalls(out1.resp.GetToolCalls(), "plat_int")
	bgText := model.Text(model.BackgroundResultBlocks(&v1.BackgroundResult{
		CallId:     "bg_call_1",
		Capability: "phone__read",
		Content:    model.TextBlocks("BG_RESULT_TOKEN_88"),
	}))
	msgs2 := []*v1.LoopMessage{
		u,
		assistantMsg(out1.resp.GetContent(), rewritten),
		toolResultMsg(rewritten[0].GetCallId(), "TOOL_RES_TOKEN_44", false),
		userMsg("STEER_USER_TOKEN_99：请改为总结要点"),
		userMsg(bgText),
	}
	req2 := baseRequest(sid, rid, name, 1, 2, msgs2)
	req2.State = out1.resp.GetState()
	req2.StateVersion = out1.resp.GetStateVersion()
	req2.Previous = &v1.LoopPreviousStep{Results: []*v1.LoopCallResult{{
		Index:      0,
		CallId:     rewritten[0].GetCallId(),
		Capability: rewritten[0].GetCapability(),
		Content:    model.TextBlocks("TOOL_RES_TOKEN_44"),
	}}}

	before := len(r.cfg.Model.Calls(name))
	out2, err := r.sendStep(ctx, req2)
	if err != nil {
		return StatusFail, "step 2: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out2.resp.GetToolCalls()) != 0 || !strings.Contains(model.Text(out2.resp.GetContent()), "LOOPTEST_INTERJECTION_DONE") {
		return StatusFail, fmt.Sprintf("step 2: unexpected reply %q", model.Text(out2.resp.GetContent()))
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) <= before {
		return StatusFail, "step 2: model was not called"
	}
	lastMsgs := calls[len(calls)-1].Messages
	toolIdx, steerIdx, bgIdx := -1, -1, -1
	for i, m := range lastMsgs {
		if m.Role == string(model.RoleTool) && strings.Contains(m.Content, "TOOL_RES_TOKEN_44") {
			toolIdx = i
		}
		if strings.Contains(m.Content, "STEER_USER_TOKEN_99") {
			steerIdx = i
		}
		if strings.Contains(m.Content, "BG_RESULT_TOKEN_88") {
			bgIdx = i
		}
	}
	if toolIdx < 0 {
		return StatusFail, "step 2: model did not see tool result"
	}
	if steerIdx < 0 {
		return StatusFail, "step 2: model did not see user interjection after tool result"
	}
	if bgIdx < 0 {
		return StatusFail, "step 2: model did not see background result after tool result"
	}
	if steerIdx <= toolIdx || bgIdx <= toolIdx {
		return StatusFail, fmt.Sprintf("step 2: interjection/background (%d/%d) did not appear after tool result (%d)", steerIdx, bgIdx, toolIdx)
	}
	return StatusPass, ""
}

// 5. repeat-step：同一请求连发两次验证不缓存，再以 attempt+1 重发验证仍给出非空调用。
func runRepeatStep(ctx context.Context, r *runner) (Status, string) {
	const name = "repeat-step"
	r.cfg.Model.beginScenario(name, func(ModelCall) ModelReply {
		return ModelReply{ToolCalls: []ModelToolCall{
			{Name: "lookup", Arguments: `{"key":"repeat_k"}`},
		}}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	u := userMsg("重复请求测试。")

	checkOne := func(label string, attempt uint32, prevCount int) (int, error) {
		req := baseRequest(sid, rid, name, attempt, 1, []*v1.LoopMessage{u})
		out, err := r.sendStep(ctx, req)
		if err != nil {
			return prevCount, fmt.Errorf("%s: %w", label, err)
		}
		if err := r.checkViolations(name); err != nil {
			return prevCount, err
		}
		tcs := out.resp.GetToolCalls()
		if len(tcs) != 1 || tcs[0].GetCapability() != "lookup" || !jsonEqual(tcs[0].GetArgumentsJson(), `{"key":"repeat_k"}`) {
			return prevCount, fmt.Errorf("%s: got tool_calls %v, want single lookup(repeat_k)", label, tcs)
		}
		now := len(r.cfg.Model.Calls(name))
		if now <= prevCount {
			return now, fmt.Errorf("%s: model call count stayed at %d (service cached the response)", label, now)
		}
		return now, nil
	}

	n, err := checkOne("first request", 1, 0)
	if err != nil {
		return StatusFail, err.Error()
	}
	n, err = checkOne("identical repeat", 1, n)
	if err != nil {
		return StatusFail, err.Error()
	}
	if _, err := checkOne("attempt+1 repeat", 2, n); err != nil {
		return StatusFail, err.Error()
	}
	return StatusPass, ""
}

// 6. repeat-with-new-input：相同 (run_id, attempt, turn, state) 重发但末尾多一条插话，模型必须看到插话。
func runRepeatWithNewInput(ctx context.Context, r *runner) (Status, string) {
	const name = "repeat-with-new-input"
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		if countToolMsgs(call.Messages) == 0 {
			return ModelReply{ToolCalls: []ModelToolCall{
				{ID: "fm_rni_1", Name: "lookup", Arguments: `{"key":"rni"}`},
			}}
		}
		if hasMessageContaining(call.Messages, "RETRY_STEER_TOKEN_77") {
			return ModelReply{Text: "LOOPTEST_RNI_SAW_NEW_INPUT"}
		}
		return ModelReply{Text: "LOOPTEST_RNI_FIRST_TRY"}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	u := userMsg("先查询一次。")
	out1, err := r.sendStep(ctx, baseRequest(sid, rid, name, 1, 1, []*v1.LoopMessage{u}))
	if err != nil {
		return StatusFail, "step 1: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out1.resp.GetToolCalls()) != 1 {
		return StatusFail, fmt.Sprintf("step 1: got %d tool_calls, want 1", len(out1.resp.GetToolCalls()))
	}

	rewritten := rewriteCalls(out1.resp.GetToolCalls(), "plat_rni")
	prev := &v1.LoopPreviousStep{Results: []*v1.LoopCallResult{{
		Index:      0,
		CallId:     rewritten[0].GetCallId(),
		Capability: rewritten[0].GetCapability(),
		Content:    model.TextBlocks("RNI_RES_1"),
	}}}
	baseMsgs := []*v1.LoopMessage{
		u,
		assistantMsg(out1.resp.GetContent(), rewritten),
		toolResultMsg(rewritten[0].GetCallId(), "RNI_RES_1", false),
	}

	// 第一次发第 2 步。
	req2a := baseRequest(sid, rid, name, 1, 2, baseMsgs)
	req2a.State = out1.resp.GetState()
	req2a.StateVersion = out1.resp.GetStateVersion()
	req2a.Previous = prev
	if _, err := r.sendStep(ctx, req2a); err != nil {
		return StatusFail, "step 2 (first): " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}

	// 相同 run_id、attempt、turn、state 重发，但 messages 末尾多一条插话。
	before := len(r.cfg.Model.Calls(name))
	req2b := baseRequest(sid, rid, name, 1, 2, append(append([]*v1.LoopMessage{}, baseMsgs...), userMsg("RETRY_STEER_TOKEN_77：补充要求")))
	req2b.State = out1.resp.GetState()
	req2b.StateVersion = out1.resp.GetStateVersion()
	req2b.Previous = prev

	out2b, err := r.sendStep(ctx, req2b)
	if err != nil {
		return StatusFail, "step 2 (with new input): " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) <= before {
		return StatusFail, "step 2 (with new input): model was not called (service cached by run/turn)"
	}
	if !hasMessageContaining(calls[len(calls)-1].Messages, "RETRY_STEER_TOKEN_77") {
		return StatusFail, "step 2 (with new input): model did not see the new interjection"
	}
	if !strings.Contains(model.Text(out2b.resp.GetContent()), "LOOPTEST_RNI_SAW_NEW_INPUT") {
		return StatusFail, fmt.Sprintf("step 2 (with new input): reply %q did not reflect fresh model output", model.Text(out2b.resp.GetContent()))
	}
	return StatusPass, ""
}

// 7. new-run-empty-state：同一 Session 下新 Run 的第一步 state 为空，模型须看到上一 Run 历史，
// 且新 Run 回答的 state 不得携带上一个 Run 的 state。
func runNewRunEmptyState(ctx context.Context, r *runner) (Status, string) {
	const name = "new-run-empty-state"
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		if hasMessageContaining(call.Messages, "NEW_RUN_PROMPT_22") {
			return ModelReply{Text: "LOOPTEST_NEW_RUN_OK"}
		}
		if countToolMsgs(call.Messages) == 0 {
			return ModelReply{ToolCalls: []ModelToolCall{
				{ID: "fm_nr_1", Name: "lookup", Arguments: `{"key":"prev_run_key"}`},
			}}
		}
		return ModelReply{Text: "PREV_RUN_REPLY_11"}
	})
	defer r.cfg.Model.endScenario(name)

	sid := r.nextID("s", name)
	rid1 := r.nextID("r1", name)
	u1 := userMsg("PREV_RUN_USER_11：请先查询。")
	out1a, err := r.sendStep(ctx, baseRequest(sid, rid1, name, 1, 1, []*v1.LoopMessage{u1}))
	if err != nil {
		return StatusFail, "prev run step 1: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out1a.resp.GetToolCalls()) != 1 {
		return StatusFail, fmt.Sprintf("prev run step 1: got %d tool_calls, want 1", len(out1a.resp.GetToolCalls()))
	}

	rewritten := rewriteCalls(out1a.resp.GetToolCalls(), "plat_nr")
	run1Msgs := []*v1.LoopMessage{
		u1,
		assistantMsg(out1a.resp.GetContent(), rewritten),
		toolResultMsg(rewritten[0].GetCallId(), "PREV_RUN_TOOL_RES_11", false),
	}
	req1b := baseRequest(sid, rid1, name, 1, 2, run1Msgs)
	req1b.State = out1a.resp.GetState()
	req1b.StateVersion = out1a.resp.GetStateVersion()
	req1b.Previous = &v1.LoopPreviousStep{Results: []*v1.LoopCallResult{{
		Index:      0,
		CallId:     rewritten[0].GetCallId(),
		Capability: rewritten[0].GetCapability(),
		Content:    model.TextBlocks("PREV_RUN_TOOL_RES_11"),
	}}}
	out1b, err := r.sendStep(ctx, req1b)
	if err != nil {
		return StatusFail, "prev run step 2: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}

	// 新 Run：同一 session_id、新 run_id、state 为空，messages 保留上一个已完成 Run 的历史。
	rid2 := r.nextID("r2", name)
	run2Msgs := append(append([]*v1.LoopMessage{}, run1Msgs...),
		assistantMsg(out1b.resp.GetContent(), nil),
		userMsg("NEW_RUN_PROMPT_22：请结合之前的结果回答。"),
	)
	before := len(r.cfg.Model.Calls(name))
	out2, err := r.sendStep(ctx, baseRequest(sid, rid2, name, 1, 1, run2Msgs))
	if err != nil {
		return StatusFail, "new run: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out2.resp.GetToolCalls()) != 0 || !strings.Contains(model.Text(out2.resp.GetContent()), "LOOPTEST_NEW_RUN_OK") {
		return StatusFail, fmt.Sprintf("new run: unexpected reply %q", model.Text(out2.resp.GetContent()))
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) <= before {
		return StatusFail, "new run: model was not called"
	}
	lastMsgs := calls[len(calls)-1].Messages
	if !hasMessageContaining(lastMsgs, "PREV_RUN_USER_11") || !hasMessageContaining(lastMsgs, "PREV_RUN_REPLY_11") {
		return StatusFail, "new run: model did not see previous run history"
	}
	// 新 Run 回答的 state 不得包含上一个 Run 的 run_id 或带入上一个 Run 的终态。
	newState := out2.resp.GetState()
	if len(newState) > 0 {
		if bytes.Contains(newState, []byte(rid1)) {
			return StatusFail, fmt.Sprintf("new run state leaked previous run_id %q", rid1)
		}
		prevFinal := out1b.resp.GetState()
		if len(prevFinal) > 0 && !bytes.Equal(prevFinal, out1a.resp.GetState()) &&
			(bytes.Equal(newState, prevFinal) || bytes.Contains(newState, prevFinal)) {
			return StatusFail, "new run state carried over previous run's state"
		}
	}
	return StatusPass, ""
}

// 8. compaction：多步 Run 第 4 步发生上下文压缩（摘要 + 最近一步助手消息及其结果），state 照常带回。
func runCompaction(ctx context.Context, r *runner) (Status, string) {
	const name = "compaction"
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		if hasMessageContaining(call.Messages, "COMPACT_SUMMARY_TOKEN_42") {
			return ModelReply{Text: "LOOPTEST_COMPACT_DONE"}
		}
		n := countToolMsgs(call.Messages)
		return ModelReply{ToolCalls: []ModelToolCall{
			{Name: "lookup", Arguments: fmt.Sprintf(`{"idx":%d,"key":"cmp_%d"}`, n, n)},
		}}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	msgs := []*v1.LoopMessage{userMsg("最初的长任务：ORIG_TASK_0")}
	var state []byte
	var stateVer string
	var prev *v1.LoopPreviousStep
	var lastAsst *v1.LoopMessage
	var lastTool *v1.LoopMessage

	for turn := uint32(1); turn <= 3; turn++ {
		req := baseRequest(sid, rid, name, 1, turn, msgs)
		req.State = state
		req.StateVersion = stateVer
		req.Previous = prev
		out, err := r.sendStep(ctx, req)
		if err != nil {
			return StatusFail, fmt.Sprintf("step %d: %v", turn, err)
		}
		if err := r.checkViolations(name); err != nil {
			return StatusFail, err.Error()
		}
		if len(out.resp.GetToolCalls()) != 1 {
			return StatusFail, fmt.Sprintf("step %d: got %d tool_calls, want 1", turn, len(out.resp.GetToolCalls()))
		}
		rewritten := rewriteCalls(out.resp.GetToolCalls(), fmt.Sprintf("plat_cmp_%d", turn))
		resText := fmt.Sprintf("CMP_RES_%d", turn)
		if turn == 3 {
			resText = "CMP_RECENT_RESULT_3"
		}
		lastAsst = assistantMsg(out.resp.GetContent(), rewritten)
		lastTool = toolResultMsg(rewritten[0].GetCallId(), resText, false)
		msgs = append(msgs, lastAsst, lastTool)
		state = out.resp.GetState()
		stateVer = out.resp.GetStateVersion()
		prev = &v1.LoopPreviousStep{Results: []*v1.LoopCallResult{{
			Index:      0,
			CallId:     rewritten[0].GetCallId(),
			Capability: rewritten[0].GetCapability(),
			Content:    model.TextBlocks(resText),
		}}}
	}

	// 第 4 步：压缩掉更早的消息，只留摘要用户消息 + 最近一步（第 3 步）的助手消息与其结果，state 照常带回。
	compactedMsgs := []*v1.LoopMessage{
		userMsg("[此前的对话已压缩为以下摘要；更早的原文不再可见]\nCOMPACT_SUMMARY_TOKEN_42：前两步已完成。"),
		lastAsst,
		lastTool,
	}
	req4 := baseRequest(sid, rid, name, 1, 4, compactedMsgs)
	req4.State = state
	req4.StateVersion = stateVer
	req4.Previous = prev

	before := len(r.cfg.Model.Calls(name))
	out4, err := r.sendStep(ctx, req4)
	if err != nil {
		return StatusFail, "step 4 (compacted): " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out4.resp.GetToolCalls()) != 0 || !strings.Contains(model.Text(out4.resp.GetContent()), "LOOPTEST_COMPACT_DONE") {
		return StatusFail, fmt.Sprintf("step 4: unexpected reply %q", model.Text(out4.resp.GetContent()))
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) <= before {
		return StatusFail, "step 4: model was not called"
	}
	lastMsgs := calls[len(calls)-1].Messages
	if !hasMessageContaining(lastMsgs, "COMPACT_SUMMARY_TOKEN_42") {
		return StatusFail, "step 4: model did not see compaction summary"
	}
	if err := checkToolPairing(lastMsgs, []string{"CMP_RECENT_RESULT_3"}); err != nil {
		return StatusFail, "step 4: " + err.Error()
	}
	return StatusPass, ""
}

// 9. state-limit：12 步带约 8 KiB 不可压缩结果的 Run，每一步的 state 不得超过 64 KiB。
func runStateLimit(ctx context.Context, r *runner) (Status, string) {
	const name = "state-limit"
	const toolSteps = 12
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		n := countToolMsgs(call.Messages)
		if n < toolSteps {
			return ModelReply{ToolCalls: []ModelToolCall{
				{Name: "lookup", Arguments: fmt.Sprintf(`{"idx":%d,"key":"big_%d"}`, n, n)},
			}}
		}
		return ModelReply{Text: "LOOPTEST_STATE_LIMIT_DONE"}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	msgs := []*v1.LoopMessage{userMsg("连续执行 12 步大结果查询。")}
	var state []byte
	var stateVer string
	var prev *v1.LoopPreviousStep

	for turn := uint32(1); turn <= toolSteps+1; turn++ {
		req := baseRequest(sid, rid, name, 1, turn, msgs)
		req.State = state
		req.StateVersion = stateVer
		req.Previous = prev
		out, err := r.sendStep(ctx, req)
		if err != nil {
			return StatusFail, fmt.Sprintf("step %d: %v", turn, err)
		}
		if err := r.checkViolations(name); err != nil {
			return StatusFail, err.Error()
		}
		if turn == toolSteps+1 {
			if len(out.resp.GetToolCalls()) != 0 || !strings.Contains(model.Text(out.resp.GetContent()), "LOOPTEST_STATE_LIMIT_DONE") {
				return StatusFail, fmt.Sprintf("final step: unexpected reply %q", model.Text(out.resp.GetContent()))
			}
			break
		}
		if len(out.resp.GetToolCalls()) != 1 {
			return StatusFail, fmt.Sprintf("step %d: got %d tool_calls, want 1", turn, len(out.resp.GetToolCalls()))
		}
		rewritten := rewriteCalls(out.resp.GetToolCalls(), fmt.Sprintf("plat_lim_%d", turn))
		payload := bigPayload(int(turn), 8192)
		msgs = append(msgs, assistantMsg(out.resp.GetContent(), rewritten), toolResultMsg(rewritten[0].GetCallId(), payload, false))
		state = out.resp.GetState()
		stateVer = out.resp.GetStateVersion()
		prev = &v1.LoopPreviousStep{Results: []*v1.LoopCallResult{{
			Index:      0,
			CallId:     rewritten[0].GetCallId(),
			Capability: rewritten[0].GetCapability(),
			Content:    model.TextBlocks(payload),
		}}}
	}
	return StatusPass, ""
}

// bigPayload 生成确定性的伪随机十六进制文本（约 size 字节）：避免被 zlib 等压缩掩盖"未剥离消息进 state"的缺陷。
func bigPayload(step, size int) string {
	var b strings.Builder
	b.Grow(size + 64)
	fmt.Fprintf(&b, "STEP_%d_PAYLOAD_", step)
	sum := sha256.Sum256([]byte(fmt.Sprintf("looptest-state-limit-%d", step)))
	for b.Len() < size {
		sum = sha256.Sum256(sum[:])
		b.WriteString(hex.EncodeToString(sum[:]))
	}
	return b.String()[:size]
}

// 10. restart：第 1 步 → 重启服务 → 带第 1 步的 state 发第 2 步，验证无进程内状态依赖。
func runRestart(ctx context.Context, r *runner) (Status, string) {
	const name = "restart"
	if r.cfg.Restart == nil {
		return StatusSkip, "no Restart hook configured"
	}
	r.cfg.Model.beginScenario(name, func(call ModelCall) ModelReply {
		if countToolMsgs(call.Messages) == 0 {
			return ModelReply{ToolCalls: []ModelToolCall{
				{ID: "fm_rst_1", Name: "lookup", Arguments: `{"key":"restart"}`},
			}}
		}
		return ModelReply{Text: "LOOPTEST_RESTART_DONE"}
	})
	defer r.cfg.Model.endScenario(name)

	sid, rid := r.nextID("s", name), r.nextID("r", name)
	u := userMsg("换进程恢复测试。")
	out1, err := r.sendStep(ctx, baseRequest(sid, rid, name, 1, 1, []*v1.LoopMessage{u}))
	if err != nil {
		return StatusFail, "step 1: " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out1.resp.GetToolCalls()) != 1 {
		return StatusFail, fmt.Sprintf("step 1: got %d tool_calls, want 1", len(out1.resp.GetToolCalls()))
	}

	if err := r.cfg.Restart(ctx); err != nil {
		return StatusFail, fmt.Sprintf("restart hook failed: %v", err)
	}

	rewritten := rewriteCalls(out1.resp.GetToolCalls(), "plat_rst")
	msgs2 := []*v1.LoopMessage{
		u,
		assistantMsg(out1.resp.GetContent(), rewritten),
		toolResultMsg(rewritten[0].GetCallId(), "RES_AFTER_RESTART_55", false),
	}
	req2 := baseRequest(sid, rid, name, 1, 2, msgs2)
	req2.State = out1.resp.GetState()
	req2.StateVersion = out1.resp.GetStateVersion()
	req2.Previous = &v1.LoopPreviousStep{Results: []*v1.LoopCallResult{{
		Index:      0,
		CallId:     rewritten[0].GetCallId(),
		Capability: rewritten[0].GetCapability(),
		Content:    model.TextBlocks("RES_AFTER_RESTART_55"),
	}}}

	before := len(r.cfg.Model.Calls(name))
	out2, err := r.sendStep(ctx, req2)
	if err != nil {
		return StatusFail, "step 2 (after restart): " + err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(out2.resp.GetToolCalls()) != 0 || !strings.Contains(model.Text(out2.resp.GetContent()), "LOOPTEST_RESTART_DONE") {
		return StatusFail, fmt.Sprintf("step 2: unexpected reply %q", model.Text(out2.resp.GetContent()))
	}
	calls := r.cfg.Model.Calls(name)
	if len(calls) <= before {
		return StatusFail, "step 2: model was not called"
	}
	if err := checkToolPairing(calls[len(calls)-1].Messages, []string{"RES_AFTER_RESTART_55"}); err != nil {
		return StatusFail, "step 2: " + err.Error()
	}
	return StatusPass, ""
}

// 11. model-failure：假模型返回 500，服务不得给出回答：非 200、连接失败，或 200 但解析不出回答
// （流式已发出响应头、随后缺少最后一行）均通过，平台对后者同样按失败处理（ADR-0062 第 4 条）。
func runModelFailure(ctx context.Context, r *runner) (Status, string) {
	const name = "model-failure"
	r.cfg.Model.beginScenario(name, func(ModelCall) ModelReply {
		return ModelReply{Status: http.StatusInternalServerError}
	})
	defer r.cfg.Model.endScenario(name)

	req := baseRequest(r.nextID("s", name), r.nextID("r", name), name, 1, 1, []*v1.LoopMessage{userMsg("模型故障测试。")})
	status, answered, err := r.sendRaw(ctx, req)
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if len(r.cfg.Model.Calls(name)) == 0 {
		return StatusFail, "model was not called"
	}
	switch {
	case err != nil:
		return StatusPass, "connection/request failed as expected"
	case answered:
		return StatusFail, "service answered the step although the model returned 500"
	case status == http.StatusOK:
		return StatusPass, "HTTP 200 without a parsable answer, treated as a failed step"
	}
	return StatusPass, fmt.Sprintf("returned HTTP %d as expected", status)
}

// 12. streaming：报告服务是否使用 NDJSON 流式回答；不流式不算失败，在结果中注明。
func runStreaming(ctx context.Context, r *runner) (Status, string) {
	const name = "streaming"
	r.cfg.Model.beginScenario(name, func(ModelCall) ModelReply {
		return ModelReply{Text: "LOOPTEST_STREAM_OK"}
	})
	defer r.cfg.Model.endScenario(name)

	req := baseRequest(r.nextID("s", name), r.nextID("r", name), name, 1, 1, []*v1.LoopMessage{userMsg("流式响应检测。")})
	out, err := r.sendStep(ctx, req)
	if err != nil {
		return StatusFail, err.Error()
	}
	if err := r.checkViolations(name); err != nil {
		return StatusFail, err.Error()
	}
	if !strings.Contains(model.Text(out.resp.GetContent()), "LOOPTEST_STREAM_OK") {
		return StatusFail, fmt.Sprintf("reply %q missing expected text", model.Text(out.resp.GetContent()))
	}
	if out.streamed {
		return StatusPass, fmt.Sprintf("streaming (application/x-ndjson, %d deltas)", out.deltas)
	}
	return StatusPass, "non-streaming (application/json)"
}
