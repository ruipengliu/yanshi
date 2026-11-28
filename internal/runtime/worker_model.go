package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
)

func (w *Worker) callModel(ctx context.Context, r *session.Run, def *agentdef.Def) error {
	if p, err := w.overQuota(ctx, "run"); err != nil || p != nil {
		if err != nil {
			return err
		}
		return w.suspendWith(ctx, &v1.RunSuspended{RunId: r.ID, Attempt: w.attempt, Reason: p.Reason(), Until: timestamppb.New(p.ResetAt)},
			w.quotaPark(p))
	}
	// 外部循环（ADR-0062）：按 Session 的业务线解析登记，没有登记是配置错误。
	loop, external := w.cfg.Loop, def.Loop != nil
	bl := w.st.Created.GetBusinessLine()
	if external {
		var err error
		if w.cfg.Loops == nil {
			err = ErrLoopNotRegistered
		} else {
			loop, err = w.cfg.Loops.Loop(bl, def.Loop.Server)
		}
		if err != nil {
			return w.fail(ctx, r, fmt.Sprintf("loop %s: %v", def.Loop.Server, err))
		}
	}
	tools, err := w.tools(ctx, def)
	if errors.Is(err, capability.ErrConfig) {
		return w.fail(ctx, r, err.Error())
	}
	if err != nil {
		// 查询设备目录或 MCP Server 的暂时性故障：本步重试，持续出错时由 StepErrorBudget 使 Run 失败。
		return fmt.Errorf("catalog: %w", err)
	}
	// 系统指令只含 AgentDef 的指令：召回放在 Run 的位置上（Transcript），使系统指令与工具定义构成的前缀
	// 在整个 Session 中不变，提供商的前缀缓存得以命中（docs/research/2026-10-agent-harness-and-system1.md）。
	view := contextView(w.st, def)
	req := &model.Request{Model: def.Model, System: def.Instructions, Messages: w.inlineImages(ctx, Transcript(view, def.Context.MaxToolResult))}
	for _, t := range tools {
		req.Tools = append(req.Tools, model.ToolSpec{Name: t.Spec.Name, Description: t.Spec.Description, InputSchema: t.Spec.InputSchema})
	}
	var limit uint64
	if external {
		// 只看本轮、没有摘要模型的外部循环（ADR-0062 第 6 条）：超出窗口时 Run 失败，不论能否找到截断点。
		if def.Context.SummaryModel == "" && (w.forceCompact || estimate(view, req, def.Context.MaxToolResult) > def.Context.Threshold()) {
			return w.fail(ctx, r, "context exceeds the window and the loop agent has no context.summary_model")
		}
		limit = loopCutLimit(view, r.ID)
	}
	if through, usePrefix := planCompactionBefore(view, req, def.Context, w.forceCompact, limit); through > 0 {
		if !usePrefix {
			req = nil
		}
		return w.compact(ctx, r, def, view, through, req)
	}
	step := LoopStep{Request: req}
	if external {
		// 外部循环收到完整的上下文视图（含他人旁听中的话与设备的结果，ADR-0062 第 7 条）。Worker 在两步之间沿用本地
		// 投影，Session 被删除后要到写入时才发现（模拟测试的 LoopRequestsAfterDeletion）；每步请求之前查一次删除记录，
		// 已删除就不再发出。删除记录先于清理写入（ADR-0015），查到时日志可能还在。内置的模型调用不查：请求只交给
		// 模型提供商，多一次存储读取不划算。
		if d := w.cfg.Store.Deletions; d != nil {
			gone, err := lifecycle.Deleted(ctx, d, w.st.SessionID)
			if err != nil {
				return err
			}
			if gone {
				w.drop()
				return nil
			}
		}
		step.SessionID, step.RunID, step.Attempt, step.Turn = w.st.SessionID, r.ID, w.attempt, uint32(r.Turns+1)
		step.EndUser, step.Agent = w.st.Created.GetEndUser(), w.st.Agent
		step.State, step.StateVersion = r.LoopState, r.LoopStateVersion
		step.Previous = previousStep(view, r.ID, def.Context.MaxToolResult)
		step.Models = def.Loop.Models
	}

	var reply *LoopReply
	var resp *model.Response
	start := time.Now()
	kind := "turn"
	if external {
		kind = "loop"
	}
	defer func() { observeModel(kind, start, resp, err) }()
	var span *tracing.Span
	if external {
		span = w.attemptSpan.Child(tracing.LoopStep)
		span.Set(tracing.Count(tracing.Turn, int(step.Turn)))
	} else {
		span = w.attemptSpan.Child(tracing.ModelCall)
	}
	// 耗时与首字延迟只用于观察，但写入日志：按 clock.Clock 计时，模拟测试中确定（ADR-0034）。
	called := w.now()
	var firstToken time.Duration
	err = w.during(ctx, r, func(ctx context.Context) error {
		var err error
		draft := live.Delta{SessionID: w.st.SessionID, RunID: r.ID, Attempt: w.attempt, AfterSeq: w.st.Seq}
		reply, err = loop.Decide(span.Context(ctx), step, func(d model.Delta) {
			if firstToken == 0 {
				firstToken = max(w.now().Sub(called), time.Millisecond)
				span.FirstToken()
			}
			draft.Text, draft.Reasoning = d.Text, d.Reasoning
			w.cfg.Live.Publish(draft)
		})
		draft.Reasoning = ""
		draft.Text, draft.End = "", true
		w.cfg.Live.Publish(draft)
		if err == nil && external {
			err = checkReply(reply, req)
		}
		return err
	})
	if span != nil {
		outcome := model.TraceOutcome(err)
		if errors.Is(err, errSuperseded) {
			outcome = tracing.Lost
		}
		span.End(outcome)
	}
	if err != nil {
		what := "model"
		if external {
			what = "loop"
		}
		return w.modelFailed(ctx, r, what, err)
	}
	resp = reply.Response
	w.modelErrors = 0
	// 在写日志之前计量：token 已经消耗，之后写日志失败（被接管、Session 被删除）也应计费（ADR-0019）。
	// 外部循环按步计量。它自报的模型用量（ADR-0056 Q6）暂不记录：用量报表还不能按 Kind 分开，混入会把自报的 token
	// 算进模型调用；且自报的模型名是外部服务填写的任意文本，用量记录不随 Session 删除。
	agent := agentdef.Label(w.st.Agent)
	if external {
		w.cfg.Meter.Loop(ctx, w.usageScope(), "use_"+w.cfg.Store.IDs(), agent, bl+"/"+def.Loop.Server, w.now())
	} else {
		w.cfg.Meter.Model(ctx, w.usageScope(), "use_"+w.cfg.Store.IDs(), agent, def.Model, resp.Usage, w.now())
	}

	// 模型给出的调用 ID 可能为空或跨轮、跨 Run 重复；一律改写为全局唯一 ID，
	// 使其可作为下游幂等键。改写后的 ID 随事件落盘，后续上下文都使用它。
	// 新 ID 继承 Session 的槽位（ADR-0051）；存储访问仍须带真实 Session，不凭 ID 外形定位。
	for _, tc := range resp.ToolCalls {
		if tc.CallId, err = w.cfg.Store.IDs.ForSession("call", w.st.SessionID); err != nil {
			return err
		}
	}
	msg := newAssistantMessage(r.ID, w.attempt, resp, w.now().Sub(called), firstToken)
	if external {
		msg.Model, msg.LoopState, msg.LoopStateVersion = "loop/"+def.Loop.Server, reply.State, reply.StateVersion
	}
	var events []*v1.Event
	// 内容安全（ADR-0021）：检查的正是将要写入日志的消息（回复、调用与截断后的思考过程，ADR-0034）。
	// 违规的回复替换为拒答文本、丢弃调用与思考过程，原文不写入日志。服务不可用时不放行：丢弃这条回复
	// （其中的调用从未执行），Run 挂起、退避后重新生成并送检（ADR-0054）。
	// 用单独的变量：err 被上面的 defer 用来记录模型调用的结果。
	v, merr := moderation.Check(ctx, w.cfg.Moderator, moderation.ForOutput(bl, w.st.SessionID, ModerationText(msg)))
	if errors.Is(merr, moderation.ErrUnavailable) {
		return w.awaitModeration(ctx, r)
	}
	if merr != nil {
		return merr
	}
	if v.Block {
		w.cfg.Logger.Warn("model output blocked by moderation", "session", w.st.SessionID, "run", r.ID, "labels", v.Labels)
		events = append(events, &v1.Event{Payload: &v1.Event_ContentModerated{ContentModerated: &v1.ContentModerated{
			RunId: r.ID, Attempt: w.attempt, Stage: string(moderation.Output), Labels: v.Labels}}})
		refuse(msg)
	}
	events = append(events, &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: msg}})
	if len(msg.GetToolCalls()) == 0 {
		events = append(events, &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: r.ID, Attempt: w.attempt}}})
	}
	return w.commit(ctx, events...)
}

// inlineImageLimit 是内联给模型的单个图片或音频的上限（16kHz 单声道 WAV 约两分钟）。
const inlineImageLimit = 4 << 20

// inlinable 报告用户消息中的媒体是否内联给模型：图片与音频（语音消息）。是否真的以图片、音频发送由提供商决定，
// 不支持的格式仍呈现为描述文本。
func inlinable(mime string) bool {
	return strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "audio/")
}

// inlineImages 把用户消息中的图片与音频工件读出并内联；读取失败或过大时保留原引用（模型看到描述文本）。
func (w *Worker) inlineImages(ctx context.Context, msgs []model.Message) []model.Message {
	if w.cfg.Artifacts == nil {
		return msgs
	}
	for i, m := range msgs {
		if m.Role != model.RoleUser {
			continue
		}
		var blocks []*v1.ContentBlock
		for _, b := range m.Content {
			media := b.GetMedia()
			id, ok := artifact.ParseURI(media.GetUri())
			if !ok || !inlinable(media.GetMimeType()) || media.GetSize() > inlineImageLimit {
				blocks = append(blocks, b)
				continue
			}
			data, err := w.readArtifact(ctx, id)
			if err != nil {
				w.cfg.Logger.Warn("inline image failed", "artifact", id, "err", err)
				blocks = append(blocks, b)
				continue
			}
			blocks = append(blocks, &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{
				MimeType: media.GetMimeType(), Name: media.GetName(), Size: media.GetSize(), Uri: media.GetUri(), Data: data,
			}}})
		}
		msgs[i].Content = blocks
	}
	return msgs
}

func (w *Worker) readArtifact(ctx context.Context, id string) ([]byte, error) {
	m, rc, err := w.cfg.Artifacts.Open(ctx, w.st.SessionID, id)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// 工件只对其所属 Session 可见。
	if m.SessionID != w.st.SessionID {
		return nil, artifact.ErrNotFound
	}
	return io.ReadAll(io.LimitReader(rc, inlineImageLimit))
}

// modelFailed 处理模型调用（含摘要）的失败：连续失败达到上限则 RunFailed，否则留待下一步重试。
// 上下文超长同样计入失败次数，并让下一步先压缩；压缩无法缓解时最终以 RunFailed 结束而不是无限重试。
func (w *Worker) modelFailed(ctx context.Context, r *session.Run, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errSuperseded) {
		return nil
	}
	if errors.Is(err, model.ErrContextOverflow) {
		w.forceCompact = true
	}
	w.modelErrors++
	if w.modelErrors >= w.cfg.MaxModelErrors {
		return w.fail(ctx, r, fmt.Sprintf("%s error: %v", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

func observeModel(kind string, start time.Time, resp *model.Response, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	metrics.ModelCallDuration.WithLabelValues(kind, outcome).Observe(metrics.Since(start))
	if resp != nil && resp.Usage != nil {
		metrics.ModelTokens.WithLabelValues("input").Add(float64(resp.Usage.GetInputTokens()))
		metrics.ModelTokens.WithLabelValues("output").Add(float64(resp.Usage.GetOutputTokens()))
		metrics.ModelTokens.WithLabelValues("cached_input").Add(float64(resp.Usage.GetCachedInputTokens()))
	}
}

// newAssistantMessage 是模型这一轮的回复将要写入日志的形式。内容安全检查的正是它（ModerationText），
// 所以"检查了什么"与"记录了什么"不会分叉。
func newAssistantMessage(runID string, attempt uint32, resp *model.Response, duration, firstToken time.Duration) *v1.AssistantMessage {
	return &v1.AssistantMessage{
		RunId: runID, Attempt: attempt, Content: resp.Content, ToolCalls: resp.ToolCalls, Model: resp.Model, Usage: resp.Usage,
		Reasoning: truncateReasoning(resp.Reasoning), DurationMs: millis(duration), FirstTokenMs: millis(firstToken),
		FinishReason: model.SafeCode(resp.FinishReason),
	}
}

// ModerationText 是送检的模型输出：回复文本加每个调用的能力名与参数，再加思考过程。调用参数可能携带以用户名义发出的内容
// （消息、写入的文件、保存的 Memory），同样需要检查。思考过程同样给人看（ADR-0034），以写入日志、展示给人的
// 形式（截断后）检查；流式推送的草稿按设计不检查（docs/design/m4-moderation.md §3 流式草稿）。
func ModerationText(m *v1.AssistantMessage) string {
	var b strings.Builder
	b.WriteString(model.Text(m.GetContent()))
	for _, tc := range m.GetToolCalls() {
		fmt.Fprintf(&b, "\n%s %s", tc.GetCapability(), tc.GetArgumentsJson())
	}
	if r := m.GetReasoning(); r != "" {
		b.WriteString("\n" + r)
	}
	return b.String()
}

// refuse 把被内容安全拦截的回复改为写入日志的形式：拒答文本，丢弃全部调用与思考过程，违规原文不进入日志；
// 模型、用量、耗时与结束原因只用于观察与计费，保留。
// 外部循环的状态同样不写入：Run 随即结束，不会有下一步（ADR-0062 第 5 条）。
func refuse(m *v1.AssistantMessage) {
	m.Content, m.ToolCalls, m.Reasoning = model.TextBlocks(moderation.Refusal), nil, ""
	m.LoopState, m.LoopStateVersion = nil, ""
}

// maxReasoning 是写入日志的思考过程上限：思考常比回复长得多，事件过大会拖慢每一次读取与推送。
const maxReasoning = 32 << 10

func truncateReasoning(s string) string {
	if t, cut := model.Truncate(s, maxReasoning); cut {
		return t + "\n…（思考过程过长，已截断）"
	}
	return s
}

func millis(d time.Duration) uint32 {
	return uint32(min(max(d.Milliseconds(), 0), math.MaxUint32))
}
