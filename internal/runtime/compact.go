package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// Compactor 生成压缩摘要。何时压缩、截断在哪里、如何提交由运行时负责（docs/design/m2-long-runs.md §3–4）；
// Compactor 只决定摘要怎么写，可按业务替换。
type Compactor interface {
	Summarize(ctx context.Context, in SummaryInput) (*model.Response, error)
}

type SummaryInput struct {
	// Model 是 "provider/model" 形式的模型引用。
	Model string
	// Previous 是上一份摘要，没有则为空。
	Previous []*v1.ContentBlock
	// History 是要压缩的历史事件（上一份摘要之后、截断点及之前）。
	History []*v1.Event
	// MaxToolResult 是单条调用结果的上限，与模型上下文中的截断一致。
	MaxToolResult int
	// TargetTokens 是摘要的目标长度。
	TargetTokens int
	// Prefix 非 nil 时是主请求在截断点之前的前缀（系统指令、工具定义、截断点及之前的上下文）：摘要请求以它开头，
	// 只在末尾追加压缩指令，从而命中主请求留下的前缀缓存。History 与 Previous 仍提供给不使用前缀的实现。
	Prefix *model.Request
}

// summaryHeader 标识摘要请求，便于脚本化模型（模拟测试）与日志识别。
const summaryHeader = "【上下文压缩】"

// IsSummaryRequest 报告 req 是否由默认 Compactor 发出：前缀模式下压缩指令是最后一条用户消息，
// 文本模式下是系统指令。
func IsSummaryRequest(req *model.Request) bool {
	if strings.HasPrefix(req.System, summaryHeader) {
		return true
	}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == model.RoleUser {
		return strings.HasPrefix(model.Text(req.Messages[n-1].Content), summaryHeader)
	}
	return false
}

// summaryFollowUp 是前缀模式的压缩指令：作为最后一条用户消息追加在与主请求相同的前缀之后。
// 要点与 summaryInstructions 相同；模型此时看到的是自己的对话，因此以"你"来指称 Agent。
const summaryFollowUp = summaryHeader + `请把以上对话（包括开头的对话摘要，如有）压缩成一份新的摘要，代替它们供你之后继续工作。

必须保留：
1. 用户的目标、约束、偏好，以及尚未完成的事项；
2. 已经执行且产生副作用的操作（如发送、写入、删除）及其结果，以及结果未知的操作——写明它们已执行或可能已执行，之后不得重复执行；
3. 已得到的关键事实、结论与数据——**把之后要用的数据直接写进摘要**（例如截至目前的累计值、按项汇总的数字、名单），不要写"之后再汇总"或"重新读取"：被压缩的原文之后不再可见；
4. 出现过的工件 ID（art_…）、文件路径、设备名与能力名称，原样保留。

省略寒暄与过程细节。用陈述句客观记录。只输出摘要：不要回答用户，不要调用工具。摘要不超过约 %d 字。`

// 摘要质量决定长任务的正确性，尤其是副作用不被重复执行（ADR-0012），修改须经评测。
const summaryInstructions = summaryHeader + `你负责压缩一个 Agent 的对话上下文。阅读上一份摘要（如有）与其后的对话记录，写一份新的摘要代替它们，供 Agent 继续工作。

必须保留：
1. 用户的目标、约束、偏好，以及尚未完成的事项；
2. 已经执行且产生副作用的操作（如发送、写入、删除）及其结果，以及结果未知的操作——写明它们已执行或可能已执行，Agent 不得重复执行；
3. 已得到的关键事实、结论与数据；
4. 出现过的工件 ID（art_…）、文件路径、设备名与能力名称，原样保留。

省略寒暄与过程细节。用陈述句客观记录，不要回答用户，不要调用工具。摘要不超过约 %d 字。`

// ModelCompactor 是默认 Compactor：把历史渲染为文本，请模型写摘要。
type ModelCompactor struct{ Model model.Provider }

func (c ModelCompactor) Summarize(ctx context.Context, in SummaryInput) (*model.Response, error) {
	if p := in.Prefix; p != nil {
		// 前缀模式：保留工具定义（去掉它们会改变前缀，缓存失效；tool_choice=none 在 TokenHub 上同样会去掉工具），
		// 靠指令要求不调用工具。模型仍然只给出调用、没有文本时，退回文本模式。
		msgs := append(slices.Clone(p.Messages), model.Message{Role: model.RoleUser, Content: model.TextBlocks(fmt.Sprintf(summaryFollowUp, in.TargetTokens))})
		resp, err := c.Model.Generate(ctx, &model.Request{Model: in.Model, System: p.System, Tools: p.Tools, Messages: msgs}, nil)
		if err != nil || model.Text(resp.Content) != "" {
			if resp != nil {
				resp.ToolCalls = nil
			}
			return resp, err
		}
	}
	return c.Model.Generate(ctx, &model.Request{
		Model:    in.Model,
		System:   fmt.Sprintf(summaryInstructions, in.TargetTokens),
		Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks(renderForSummary(in))}},
	}, nil)
}

// renderForSummary 把历史渲染为纯文本。不使用结构化的工具消息，因为被压缩的历史可能含有
// 永远不会有结果的调用（Run 被中断），而且不必向摘要请求声明工具。
func renderForSummary(in SummaryInput) string {
	var b strings.Builder
	if len(in.Previous) > 0 {
		fmt.Fprintf(&b, "<上一份摘要>\n%s\n</上一份摘要>\n\n", blocksText(in.Previous))
	}
	b.WriteString("<对话记录>\n")
	for _, e := range in.History {
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			fmt.Fprintf(&b, "%s %s\n", inputSpeaker("[用户]", p.RunRequested.GetFromCall()), blocksText(p.RunRequested.GetInput()))
		case *v1.Event_Steered:
			fmt.Fprintf(&b, "%s %s\n", inputSpeaker("[用户插话]", p.Steered.GetFromCall()), blocksText(p.Steered.GetInput()))
		case *v1.Event_CallTranscript:
			fmt.Fprintf(&b, "%s\n", model.CallTranscriptText(p.CallTranscript))
		case *v1.Event_AssistantMessage:
			if t := blocksText(p.AssistantMessage.GetContent()); t != "" {
				fmt.Fprintf(&b, "[助手] %s\n", t)
			}
			for _, tc := range p.AssistantMessage.GetToolCalls() {
				fmt.Fprintf(&b, "[调用 %s] %s %s\n", tc.GetCallId(), tc.GetCapability(), tc.GetArgumentsJson())
			}
		case *v1.Event_ToolResult:
			kind := "结果"
			if p.ToolResult.GetIsError() {
				kind = "失败"
			}
			fmt.Fprintf(&b, "[%s %s] %s\n", kind, p.ToolResult.GetCallId(), blocksText(truncateBlocks(p.ToolResult.GetContent(), in.MaxToolResult)))
		}
	}
	b.WriteString("</对话记录>")
	return b.String()
}

// inputSpeaker 注明 Call 派生的输入是语音助手转述的任务，而不是用户的原话。
func inputSpeaker(label, fromCall string) string {
	if fromCall != "" {
		return "[语音助手转述的任务]"
	}
	return label
}

func blocksText(blocks []*v1.ContentBlock) string {
	var parts []string
	for _, c := range blocks {
		if m := c.GetMedia(); m != nil {
			parts = append(parts, model.DescribeMedia(m))
		} else if u := c.GetUiContext(); u != nil {
			parts = append(parts, model.UIContextText(u))
		} else {
			parts = append(parts, c.GetText().GetText())
		}
	}
	return strings.Join(parts, "")
}

// eventTokens 估算一个历史事件在模型上下文中占用的 token 数，与 Transcript 的呈现一致。
func eventTokens(e *v1.Event, maxToolResult int) int {
	switch p := e.GetPayload().(type) {
	case *v1.Event_RunRequested:
		return model.EstimateMessage(model.Message{Content: model.InputBlocks(p.RunRequested.GetInput(), p.RunRequested.GetFromCall())})
	case *v1.Event_Steered:
		return model.EstimateMessage(model.Message{Content: model.InputBlocks(p.Steered.GetInput(), p.Steered.GetFromCall())})
	case *v1.Event_CallTranscript:
		return model.EstimateMessage(model.Message{Content: model.TextBlocks(model.CallTranscriptText(p.CallTranscript))})
	case *v1.Event_AssistantMessage:
		m := p.AssistantMessage
		return model.EstimateMessage(model.Message{Content: m.GetContent(), ToolCalls: m.GetToolCalls()})
	case *v1.Event_ToolResult:
		return model.EstimateMessage(toolMessage(p.ToolResult, maxToolResult))
	}
	return 0
}

// estimate 估算 req 的大小。若压缩之后有过模型调用，用其真实用量校准：
// 上次输入 + 上次输出 + 其后新增事件，取与纯估算的较大者（宁可早压缩）。
func estimate(st *session.State, req *model.Request, maxToolResult int) int {
	n := model.EstimateRequest(req)
	for i := len(st.History) - 1; i >= 0; i-- {
		e := st.History[i]
		m := e.GetAssistantMessage()
		if m == nil {
			continue
		}
		if e.GetSeq() < st.CompactedAt || m.GetUsage().GetInputTokens() == 0 {
			break // 该用量测得于压缩之前，不代表当前上下文
		}
		cal := int(m.GetUsage().GetInputTokens() + m.GetUsage().GetOutputTokens())
		for _, after := range st.History[i+1:] {
			cal += eventTokens(after, maxToolResult)
		}
		return max(n, cal)
	}
	return n
}

// planCompaction 决定是否压缩以及截断到哪个 seq；返回 0 表示不压缩。
//
// 截断点须满足：闭合边界（session.State.CanCut）；不是最后一个历史事件（最新的输入或结果保留原文）；
// 上一份摘要加上被压缩的历史不超过阈值（摘要请求本身不能超限）。在此之上尽量保留 KeepRecent 的原文；
// 做不到（或 force）时退到满足条件的最晚截断点。每次压缩严格推进 through_seq，因此反复压缩必然终止。
//
// 摘要请求优先以主请求的前缀开头（命中前缀缓存），此时系统指令与工具定义也占用它的预算；按此预算找不到
// 截断点时（窗口相对工具定义很小），退回不带前缀的文本模式与原预算，usePrefix 为 false。
func planCompaction(st *session.State, req *model.Request, cfg agentdef.Context, force bool) (through uint64, usePrefix bool) {
	if !force && estimate(st, req, cfg.MaxToolResult) <= cfg.Threshold() {
		return 0, false
	}
	h := st.History
	if len(h) < 2 {
		return 0, false
	}
	sizes := make([]int, len(h))
	total := 0
	for i, e := range h {
		sizes[i] = eventTokens(e, cfg.MaxToolResult)
		total += sizes[i]
	}
	text := cfg.Threshold() - model.EstimateTokens(st.Compaction.GetSummary()) - model.EstimateText(summaryInstructions)
	overhead := model.EstimateRequest(&model.Request{System: req.System, Tools: req.Tools})
	if cut := pickCut(st, sizes, total, text-overhead, cfg.KeepRecent, force); cut > 0 {
		return cut, true
	}
	return pickCut(st, sizes, total, text, cfg.KeepRecent, force), false
}

// pickCut 在摘要请求预算 budget 内选择截断点：尽量保留 keep 的原文，做不到时退到最晚的可行截断点。
func pickCut(st *session.State, sizes []int, total, budget, keep int, force bool) uint64 {
	h := st.History
	if force {
		keep = 0
	}
	var best, fallback uint64
	prefix := 0
	for i, e := range h[:len(h)-1] {
		prefix += sizes[i]
		if prefix > budget {
			break
		}
		if !st.CanCut(e.GetSeq()) {
			continue
		}
		fallback = e.GetSeq()
		if total-prefix >= keep {
			best = e.GetSeq()
		}
	}
	if best == 0 {
		best = fallback
	}
	return best
}

// compact 生成摘要并提交 ContextCompacted；一次 compact 是一个完整的 Step。
// compact 生成摘要并提交 ContextCompacted；main 非 nil 时摘要请求以它的前缀开头（见 planCompaction）。
func (w *Worker) compact(ctx context.Context, r *session.Run, def *agentdef.Def, through uint64, main *model.Request) error {
	var hist []*v1.Event
	for _, e := range w.st.History {
		if e.GetSeq() > through {
			break
		}
		hist = append(hist, e)
	}
	in := SummaryInput{
		Model: def.Model, Previous: w.st.Compaction.GetSummary(), History: hist,
		MaxToolResult: def.Context.MaxToolResult, TargetTokens: def.Context.Window / 16,
	}
	if main != nil {
		in.Prefix = &model.Request{System: main.System, Tools: main.Tools,
			Messages: w.inlineImages(ctx, transcriptThrough(w.st, def.Context.MaxToolResult, through))}
	}
	if def.Context.SummaryModel != "" {
		in.Model = def.Context.SummaryModel
	}
	var resp *model.Response
	start := time.Now()
	var err error
	defer func() { observeModel("summary", start, resp, err) }()
	err = w.during(ctx, r, func(ctx context.Context) error {
		var err error
		resp, err = w.cfg.Compactor.Summarize(ctx, in)
		if err == nil && model.Text(resp.Content) == "" {
			err = errors.New("empty summary")
		}
		return err
	})
	if err != nil {
		return w.modelFailed(ctx, r, "summary", err)
	}
	w.modelErrors, w.forceCompact = 0, false
	w.cfg.Meter.Model(ctx, w.usageScope(), "use_"+w.cfg.Store.IDs(), agentdef.Label(w.st.Agent), in.Model, resp.Usage, w.now())
	w.cfg.Logger.Info("context compacted", "session", w.st.SessionID, "run", r.ID, "through_seq", through, "events", len(hist))
	return w.commit(ctx, &v1.Event{Payload: &v1.Event_ContextCompacted{ContextCompacted: &v1.ContextCompacted{
		RunId: r.ID, Attempt: w.attempt, ThroughSeq: through, Summary: resp.Content, Model: resp.Model, Usage: resp.Usage,
	}}})
}
