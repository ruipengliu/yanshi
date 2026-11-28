package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/askuser"
	"yanshi/internal/capability"
	"yanshi/internal/capability/capname"
	"yanshi/internal/delegation"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/session"
)

func (w *Worker) toolResult(r *session.Run, callID string, content []*v1.ContentBlock, isErr bool) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{
		RunId: r.ID, Attempt: w.attempt, CallId: callID, Content: content, IsError: isErr,
	}}}
}

func (w *Worker) target() capability.Target {
	return capability.Target{
		Scope:     node.Scope{BusinessLine: w.st.Created.GetBusinessLine(), EndUser: w.st.Created.GetEndUser()},
		SessionID: w.st.SessionID,
	}
}

const readOnlyChanged = "能力的声明在调用开始后发生了变化，没有执行；请重新发起调用"

// advanceCall 推进一次 Capability 调用的一个阶段：
// 审批（m1 设计 §4）→ 进程内执行（m0 设计 §4）或路由派发（m1 设计 §3）。
func (w *Worker) advanceCall(ctx context.Context, r *session.Run, def *agentdef.Def, c *session.Call) error {
	id, name := c.Call.GetCallId(), c.Call.GetCapability()
	if c.Dispatched() {
		return w.awaitDispatched(ctx, r, c)
	}
	// 已执行但尚未提交的结果优先重试落盘；声明变化不能把实际执行过的调用报告成未执行。
	if res := w.executed[id]; res != nil {
		return w.commitExecuted(ctx, id, res)
	}
	tool, err := w.resolve(ctx, def, name)
	if err != nil {
		return err
	}
	if tool == nil {
		return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(fmt.Sprintf("capability %q is not available", name)), true))
	}
	startedByMe := c.Started() && c.StartedAttempts[len(c.StartedAttempts)-1] == w.attempt
	// 必须在审批与重新写 Started 之前检查：声明降级也可能提高风险，但已开始的调用不能再请求审批。
	if startedByMe && c.ReadOnlyInAttempt(w.attempt) && !tool.Spec.ReadOnly {
		return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(readOnlyChanged), true))
	}
	// 参数不合法的调用不去打扰用户，也不派发、不执行：错误作为结果交还模型，由它改正。
	if err := checkArguments(c.Call.GetArgumentsJson()); err != nil {
		return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(err.Error()), true))
	}
	// 注定被能力拒绝的调用同样不审批、不执行（capability.Checker，如 Memory 的健康许可）。只在第一次处理时检查：
	// 已请求过审批或已开始的调用照常走完。
	if ch, ok := tool.Local.(capability.Checker); ok && c.Approval == nil && !c.Started() {
		if err := ch.Check(ctx, w.invocation(r, def, id, c)); err != nil {
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(err.Error()), true))
		}
	}

	// 需要审批：高风险能力；或 Memory 写入闸门判定（ADR-0023）；已经请求过审批的调用一律走完审批，
	// 即使之后的上下文变化使闸门不再触发。闸门只在调用开始前判定：开始之后到达的插话（例如附带界面上下文）
	// 可能使上下文变为含外部内容，此时已开始的调用不能再回头请求审批（模拟测试发现）。
	summary := approvalSummary(tool, c.Call)
	gateSummary, gated := "", false
	if !c.Started() {
		gateSummary, gated = memoryGate(w.st, c.Call)
		// schedule 与 taint_gated 的 MCP 写工具经 ApprovalSummarizer 复用 memory_save 的来源判断（ADR-0061）：
		// 上下文含非用户原话且摘要非空时，写入须用户批准；空摘要不审批。先取摘要，未受约束的工具（多数 MCP 工具）
		// 不必遍历历史判断来源。
		if summarizer, ok := tool.Local.(capability.ApprovalSummarizer); ok {
			if s := summarizer.ApprovalSummary(w.invocation(r, def, id, c)); s != "" && tainted(w.st) {
				gateSummary, gated = s, true
			}
		}
	}
	if gated {
		summary = gateSummary
	}
	if tool.RequiresApproval() || gated || c.Approval != nil {
		switch {
		case c.Approval == nil:
			deadline := w.now().Add(w.cfg.ApprovalTimeout)
			return w.suspend(ctx, r, deadline, &v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{
				RunId: r.ID, Attempt: w.attempt, CallId: id, Summary: summary, Deadline: timestamppb.New(deadline),
			}}})
		case c.AwaitingApproval() && !w.now().Before(c.Approval.Deadline):
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks("approval timed out: the user did not respond; the call was not executed"), true))
		case c.AwaitingApproval():
			return w.suspend(ctx, r, c.Approval.Deadline)
		case !c.Approval.Approved:
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks("denied: the user rejected this call; it was not executed"), true))
		}
	}

	if tool.Local == nil {
		nodeID := tool.NodeID
		if tool.NodeID == askuser.NodeID {
			// 不合法的提问不去打扰用户：错误作为结果交还模型，由它改正。
			if _, err := askuser.Parse(c.Call.GetArgumentsJson()); err != nil {
				return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(err.Error()), true))
			}
		}
		if tool.Agent != "" {
			if reason := w.delegationRefused(r, def, c); reason != "" {
				return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(reason), true))
			}
			child, err := delegation.ChildSessionID(w.st.SessionID, id, w.st.Created.GetEndUser())
			if err != nil {
				return err
			}
			nodeID = delegation.NodeID(child)
		}
		deadline := w.now().Add(tool.Timeout)
		return w.commit(ctx, &v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{
			RunId: r.ID, Attempt: w.attempt, CallId: id, NodeId: nodeID, Deadline: timestamppb.New(deadline), ReadOnly: tool.Spec.ReadOnly,
		}}})
	}

	cp := tool.Local
	if !startedByMe {
		if c.Started() && !tool.Spec.Idempotent {
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(
				"outcome unknown: the call was interrupted by a failure and may or may not have taken effect; it was not retried"), true))
		}
		return w.commit(ctx, &v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{
			RunId: r.ID, Attempt: w.attempt, CallId: id, ReadOnly: tool.Spec.ReadOnly,
		}}})
	}

	var out capability.Result
	err = w.during(ctx, r, func(ctx context.Context) error {
		inv := w.invocation(r, def, id, c)
		var err error
		if ri, ok := cp.(capability.ResultInvoker); ok {
			out, err = ri.InvokeResult(ctx, inv)
		} else {
			out.Content, err = cp.Invoke(ctx, inv)
		}
		if err == nil && out.Page != nil {
			out.Content, err = w.attestedPage(ctx, inv, out.Page)
		}
		return err
	})
	var res *v1.Event
	switch {
	case err == nil:
		res = w.toolResult(r, id, out.Content, false)
		res.GetToolResult().Attested = out.Page != nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errSuperseded):
		return nil
	default:
		res = w.toolResult(r, id, model.TextBlocks(err.Error()), true)
	}
	if w.executed == nil {
		w.executed = map[string]*v1.Event{}
	}
	w.executed[id] = res
	return w.commitExecuted(ctx, id, res)
}

// invocation 是进程内调用的输入。
func (w *Worker) invocation(r *session.Run, def *agentdef.Def, id string, c *session.Call) capability.Invocation {
	return capability.Invocation{
		SessionID: w.st.SessionID, RunID: r.ID, CallID: id, Arguments: c.Call.GetArgumentsJson(),
		BusinessLine: w.st.Created.GetBusinessLine(), EndUser: w.st.Created.GetEndUser(), State: w.st, Agent: def,
	}
}

// commitExecuted 写入已执行调用的结果；写入成功（或 Attempt 已失效、结果作废）后不再保留。
func (w *Worker) commitExecuted(ctx context.Context, id string, res *v1.Event) error {
	if err := w.commit(ctx, res); err != nil {
		return err
	}
	delete(w.executed, id)
	return nil
}

// tools 返回本 Run 可用的工具。委托派生的子 Session 只能用自身白名单与每个祖先白名单都允许的工具
// （docs/design/m5-multi-agent.md §2）：委托不能借子 Agent 扩大权限。每个祖先的工具都按当前目录重新解析再按名取交
// （设备与 MCP 的目录有缓存），之后才接入、祖先白名单也允许的设备照常可用。
func (w *Worker) tools(ctx context.Context, def *agentdef.Def) ([]capability.Tool, error) {
	tools, err := w.cfg.Catalog.Tools(ctx, w.target(), def.Capabilities)
	if err != nil {
		return nil, err
	}
	for _, ref := range w.st.Created.GetParent().GetAncestors() {
		// 祖先的版本取不到（被移出注册表）时无法确定交集：作为配置错误让 Run 失败，而不是悄悄失去全部工具
		// （评审 §11 问题 11）。
		a, err := w.cfg.Agents.Get(ref)
		if err != nil {
			return nil, fmt.Errorf("%w: ancestor agent %s@%s: %v", capability.ErrConfig, ref.GetName(), ref.GetVersion(), err)
		}
		theirs, err := w.cfg.Catalog.Tools(ctx, w.target(), a.Capabilities)
		if err != nil {
			return nil, err
		}
		names := map[string]bool{}
		for _, t := range theirs {
			names[t.Spec.Name] = true
		}
		tools = slices.DeleteFunc(tools, func(t capability.Tool) bool { return !names[t.Spec.Name] })
	}
	return tools, nil
}

// resolve 按名称解析本 Run 可用的工具；不存在时返回 nil。
func (w *Worker) resolve(ctx context.Context, def *agentdef.Def, name string) (*capability.Tool, error) {
	tools, err := w.tools(ctx, def)
	if err != nil {
		return nil, err
	}
	for i := range tools {
		if tools[i].Spec.Name == name {
			return &tools[i], nil
		}
	}
	return nil, nil
}

// checkArguments 检查模型给出的调用参数：去掉首尾空白后为空时按 {} 处理（与 openaicompat、mcpcap 一致），其余必须是
// JSON 对象。不合法时的错误信息会作为调用结果交还模型，使其改正，而不是让 Go 的序列化报错从下游冒出来。
func checkArguments(args string) error {
	s := strings.TrimSpace(args)
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return fmt.Errorf("arguments are not a valid JSON object: %v; call the tool again with arguments that follow its parameter schema", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return errors.New("arguments are not a valid JSON object: the arguments must be an object; call the tool again with arguments that follow its parameter schema")
	}
	return nil
}

func approvalSummary(t *capability.Tool, c *v1.ToolCall) string {
	where := "云端"
	if t.NodeID != "" && !capname.IsSandboxTool(t.Spec.Name) {
		label, _, _ := capname.CutRouted(t.Spec.Name)
		where = "设备 " + label
	}
	args, cut := model.Truncate(c.GetArgumentsJson(), 200)
	if cut {
		args += "…"
	}
	return fmt.Sprintf("在%s上执行 %s，参数 %s", where, t.Spec.Name, args)
}
