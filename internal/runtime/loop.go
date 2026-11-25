package runtime

import (
	"context"
	"errors"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// Loop 决定 Run 的下一步：给出回复与调用请求（CONTEXT.md 的 Loop，ADR-0041）。它只做决定：上下文组装、工具解析与
// 祖先交集、配额、压缩、调用 ID、内容安全与写日志都留在 Worker 中，不区分循环是内置的还是外部的（ADR-0062 第 9 条）。
// 内置实现 ModelLoop 把平台组装的请求原样交给模型，请求逐字节不变，前缀缓存照常命中；外部循环由 Loops 按登记解析。
type Loop interface {
	// Decide 给出这一步的回复。onDelta 接收流式增量，只用于实时推送、不写日志。返回的回复由 Worker 校验（外部循环）、
	// 改写调用 ID、送检后写入日志；返回错误按模型错误处理（MaxModelErrors，上下文超长时下一步先压缩）。
	Decide(ctx context.Context, step LoopStep, onDelta func(model.Delta)) (*LoopReply, error)
}

// LoopStep 是一步的输入。Request 之外的字段只对外部循环有意义（loop.proto 的 LoopStepRequest）。
type LoopStep struct {
	// Request 是平台组装的模型请求：系统指令只含 AgentDef 的指令，召回在 Run 的位置上，Run 内的上下文只在尾部追加。
	Request *model.Request

	SessionID string
	RunID     string
	Attempt   uint32
	// Turn 是这一步在 Run 中的序号，从 1 开始。
	Turn    uint32
	EndUser string
	Agent   *v1.AgentRef
	// State 与 StateVersion 是本 Run 上一条写入日志的回答带的状态（Run 的第一步为空，ADR-0062 第 2 条）。
	State        []byte
	StateVersion string
	// Previous 是上一步回答中每个调用的结果，按回答中的原顺序（ADR-0062 第 5 条）。
	Previous []PreviousCall
	// Models 是外部服务经内核模型端点可用的模型（AgentDef 的 loop.models，评测时已被覆盖；ADR-0063 第 3 条）。
	Models []string
}

// PreviousCall 是上一步回答中的一个调用及其结果。
type PreviousCall struct {
	Call    *v1.ToolCall
	Content []*v1.ContentBlock
	IsError bool
}

// LoopReply 是一步的回答。
type LoopReply struct {
	*model.Response
	// State 与 StateVersion 随这一步的 AssistantMessage 写入日志（外部循环；内置循环为空）。
	State        []byte
	StateVersion string
	// Reported 是外部服务自报的模型用量（ADR-0056 Q6）。暂不记录，见 Worker.callModel 的计量。
	Reported []ReportedUsage
}

// ReportedUsage 是外部服务自己调用一个模型的用量。
type ReportedUsage struct {
	Model string
	Usage *v1.Usage
}

// Loops 按 (业务线, 名称) 解析外部循环的登记（loops/*.yaml，ADR-0062 第 7 条）。
type Loops interface {
	// Loop 返回该业务线登记的外部循环；没有登记时返回 ErrLoopNotRegistered。
	Loop(businessLine, name string) (Loop, error)
}

// ErrLoopNotRegistered 表示 AgentDef 指向的外部循环在 Session 的业务线中没有登记：配置错误，Run 失败。
var ErrLoopNotRegistered = errors.New("external loop is not registered for this business line")

// ModelLoop 是内置循环：调用模型。
type ModelLoop struct{ Model model.Provider }

func (l ModelLoop) Decide(ctx context.Context, step LoopStep, onDelta func(model.Delta)) (*LoopReply, error) {
	resp, err := l.Model.Generate(ctx, step.Request, onDelta)
	if err != nil {
		return nil, err
	}
	return &LoopReply{Response: resp}, nil
}

// errInvalidReply 标记外部循环不合法的回答：不写日志，与模型错误同口径计数（ADR-0062 第 4 条）。
var errInvalidReply = errors.New("invalid loop reply")

// checkReply 校验外部循环的回答：调用只能指向本步请求中的工具，回复只能是文本，状态不超过上限。
func checkReply(reply *LoopReply, req *model.Request) error {
	if reply == nil || reply.Response == nil {
		return fmt.Errorf("%w: empty", errInvalidReply)
	}
	if len(reply.State) > session.MaxLoopState {
		return fmt.Errorf("%w: state of %d bytes exceeds %d", errInvalidReply, len(reply.State), session.MaxLoopState)
	}
	tools := map[string]bool{}
	for _, t := range req.Tools {
		tools[t.Name] = true
	}
	for _, tc := range reply.ToolCalls {
		if !tools[tc.GetCapability()] {
			return fmt.Errorf("%w: call to a tool not in the request", errInvalidReply)
		}
	}
	for _, b := range reply.Content {
		if b.GetText() == nil {
			return fmt.Errorf("%w: reply content must be text", errInvalidReply)
		}
	}
	return nil
}

// previousStep 返回本 Run 最近一条回答中的调用及其结果。由外部循环驱动的 Run，压缩不越过最近一条回答
// （loopCutLimit），因此它与其后的结果都在 History 中。
func previousStep(st *session.State, runID string, maxToolResult int) []PreviousCall {
	var last *v1.AssistantMessage
	results := map[string]*v1.ToolResult{}
	for _, e := range st.History {
		if m := e.GetAssistantMessage(); m != nil && m.GetRunId() == runID {
			last, results = m, map[string]*v1.ToolResult{}
		}
		if res := e.GetToolResult(); res != nil && last != nil {
			results[res.GetCallId()] = res
		}
	}
	var out []PreviousCall
	for _, tc := range last.GetToolCalls() {
		p := PreviousCall{Call: tc}
		if res := results[tc.GetCallId()]; res != nil {
			// 与模型上下文中的结果相同的截断与旁听展开（toolMessage），错误另以 IsError 标记、不加文字前缀。
			p.Content = model.ExpandCaptures(truncateBlocks(res.GetContent(), maxToolResult))
			p.IsError = res.GetIsError()
		}
		out = append(out, p)
	}
	return out
}

// loopCutLimit 是由外部循环驱动的 Run 的压缩上限：截断点必须在本 Run 最近一条回答之前（ADR-0062 第 5 条），
// 使"上一步"与它的结果保留原文。0 表示不限（Run 还没有回答）。
func loopCutLimit(st *session.State, runID string) uint64 {
	var seq uint64
	for _, e := range st.History {
		if m := e.GetAssistantMessage(); m != nil && m.GetRunId() == runID {
			seq = e.GetSeq()
		}
	}
	return seq
}
