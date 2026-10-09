package runtime

import (
	"context"
	"errors"
	"fmt"
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
}

// summaryHeader 标识摘要请求，便于脚本化模型（模拟测试）与日志识别。
const summaryHeader = "【上下文压缩】"

// IsSummaryRequest 报告 req 是否由默认 Compactor 发出。
func IsSummaryRequest(req *model.Request) bool { return strings.HasPrefix(req.System, summaryHeader) }

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
			fmt.Fprintf(&b, "[用户] %s\n", blocksText(p.RunRequested.GetInput()))
		case *v1.Event_Steered:
			fmt.Fprintf(&b, "[用户插话] %s\n", blocksText(p.Steered.GetInput()))
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

func blocksText(blocks []*v1.ContentBlock) string {
	var parts []string
	for _, c := range blocks {
		if m := c.GetMedia(); m != nil {
			parts = append(parts, model.DescribeMedia(m))
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
		return model.EstimateMessage(model.Message{Content: p.RunRequested.GetInput()})
	case *v1.Event_Steered:
		return model.EstimateMessage(model.Message{Content: p.Steered.GetInput()})
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
func planCompaction(st *session.State, req *model.Request, cfg agentdef.Context, force bool) uint64 {
	if !force && estimate(st, req, cfg.MaxToolResult) <= cfg.Threshold() {
		return 0
	}
	h := st.History
	if len(h) < 2 {
		return 0
	}
	sizes := make([]int, len(h))
	total := 0
	for i, e := range h {
		sizes[i] = eventTokens(e, cfg.MaxToolResult)
		total += sizes[i]
	}
	budget := cfg.Threshold() - model.EstimateTokens(st.Compaction.GetSummary()) - model.EstimateText(summaryInstructions)
	keep := cfg.KeepRecent
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
func (w *Worker) compact(ctx context.Context, r *session.Run, def *agentdef.Def, through uint64) error {
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
