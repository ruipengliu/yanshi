package sim

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/card"
	"yanshi/internal/eventlog"
	"yanshi/internal/faults"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/sdk/nodesdk"
)

const redoProbePrefix = "重答模拟问题 [redo-input:"

type redoRetry struct {
	inputID  string
	expected string
	runID    string
}

func redoReplyMarker(runID string) string { return "[redo-reply:" + runID + "]" }

func (w *World) maybeRedo() error {
	// 原客户端槽占 6%，其中 40% 重答；保留平均频率，但不能抢走普通动作或跳过其抽样。
	if !w.opts.Redo || w.redoRNG.Float64() >= 0.024 {
		return nil
	}
	// probe 提交、审核、存储故障和入队后的 Worker 也会抽样，整个附加动作都须隔离。
	legacy := w.rng
	w.rng = w.redoRNG
	defer func() { w.rng = legacy }()
	return w.redoStep()
}

func (w *World) redoStep() error {
	ctx := context.Background()
	sid := w.sessions[w.redoRNG.IntN(len(w.sessions))]
	st, err := w.svc.Load(ctx, sid)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return nil
		}
		return err
	}
	// 旧动作从完整日志选择，既覆盖有 seq 的直读，也覆盖旧客户端的扫描路径。
	if len(st.SupersededCardCalls) > 0 && w.redoRNG.Float64() < 0.3 {
		events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
		if err != nil {
			return err
		}
		var candidates []*v1.Event
		for _, e := range events {
			if st.SupersededCardCalls[e.GetToolResult().GetCallId()] && card.Find(e.GetToolResult().GetContent()) != nil {
				candidates = append(candidates, e)
			}
		}
		if len(candidates) > 0 {
			e := candidates[w.redoRNG.IntN(len(candidates))]
			c := card.Find(e.GetToolResult().GetContent())
			a := &v1.CardAction{CardId: c.GetCardId(), CallId: c.GetCallId(), Action: "ok"}
			if w.redoRNG.Float64() < 0.5 {
				a.Seq = e.GetSeq()
			}
			return w.submitInput(sid, []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: a}}})
		}
	}
	if old, ok := w.redoRetries[sid]; ok && w.redoRNG.Float64() < 0.3 && st.Input(old.inputID) != nil {
		return w.tryRedo(sid, old, true)
	}
	expected := "no-run"
	if len(st.Runs) > 0 {
		expected = st.Runs[len(st.Runs)-1].ID
	}
	if st.Closed == nil && st.Active() == nil && st.RedoBlocker(expected) != "" && w.redoRNG.Float64() < 0.6 {
		w.nextRedo++
		return w.submitInput(sid, model.TextBlocks(fmt.Sprintf("%s%d]", redoProbePrefix, w.nextRedo)))
	}
	if w.redoRNG.Float64() < 0.25 {
		expected = "stale-" + expected
	}
	w.nextRedo++
	return w.tryRedo(sid, redoRetry{inputID: fmt.Sprintf("redo-%d", w.nextRedo), expected: expected}, false)
}

func (w *World) tryRedo(sid string, retry redoRetry, mustDuplicate bool) error {
	w.afterEnqueue = func() error {
		if w.redoRNG.Float64() >= 0.3 {
			return nil
		}
		w.Stats.EnqueueInterleavings++
		return w.stepWorker(w.redoRNG.IntN(len(w.workers)))
	}
	defer func() { w.afterEnqueue = nil }()
	res, err := w.svc.Redo(context.Background(), sid, retry.inputID, retry.expected)
	var sf *storeFault
	if errors.As(err, &sf) {
		// 已追加却丢回包时，同 ID 重试必须命中；沿用原子唤醒的存储故障注入。
		if sf.written {
			mustDuplicate = true
			w.Stats.UncertainRetries++
			res, err = w.svc.Redo(context.Background(), sid, retry.inputID, retry.expected)
		}
		if errors.As(err, &sf) {
			return nil
		}
	}
	if err != nil {
		if !mustDuplicate && errors.Is(err, service.ErrConflict) {
			w.Stats.RedoConflicts++
			return nil
		}
		if !mustDuplicate && (w.submitModerated(err) || w.submitOverQuota(err) || errors.Is(err, service.ErrNotFound)) {
			return nil
		}
		return fmt.Errorf("redo: %w", err)
	}
	if mustDuplicate && (!res.Duplicate || retry.runID != "" && res.RunID != retry.runID) {
		return fmt.Errorf("invariant: redo retry changed its result")
	}
	if res.Steered || res.Answered != "" {
		return fmt.Errorf("invariant: redo became steer or answer")
	}
	if res.Duplicate {
		w.Stats.RedoDuplicates++
	} else {
		w.Stats.Redos++
		if err := w.checkNewRunVersion(sid); err != nil {
			return err
		}
	}
	retry.runID = res.RunID
	w.redoRetries[sid] = retry
	w.tracef("redo %s → %s duplicate=%v", sid, res.RunID, res.Duplicate)
	return nil
}

func (w *World) rejectedRedoCard(sid string, input []*v1.ContentBlock) bool {
	st, err := w.store.Load(context.Background(), sid)
	if err != nil || st.Closed != nil {
		return false
	}
	for _, b := range input {
		if a := b.GetCardAction(); a != nil && st.SupersededCardCalls[a.GetCallId()] {
			return true
		}
	}
	return false
}

// checkRedos 用接受前的日志前缀复核资格与失效动作，不以最终投影替代历史时刻。
func checkRedos(events []*v1.Event) error {
	st := &session.State{}
	for _, e := range events {
		if m := e.GetRunRequested(); m.GetRedoOf() != "" {
			if reason := st.RedoBlocker(m.GetRedoOf()); reason != "" {
				return fmt.Errorf("redo %s accepted despite %s", m.GetRunId(), reason)
			}
		}
		if a := nodesdk.CardActionOf(e); a != nil && st.SupersededCardCalls[a.GetCallId()] {
			return fmt.Errorf("superseded card action accepted at %d", e.GetSeq())
		}
		for _, b := range e.GetToolResult().GetContent() {
			if a := b.GetCardAction(); a != nil && st.SupersededCardCalls[a.GetCallId()] {
				return fmt.Errorf("superseded card action in answer at %d", e.GetSeq())
			}
		}
		if err := st.Apply(e); err != nil {
			return err
		}
	}
	return nil
}

// redoModelState 在模型实际收到请求时检查，包括摘要的前缀和文本 fallback。
func (w *World) redoModelState(ctx context.Context, req *model.Request) (*session.State, error) {
	sid := faults.SessionFrom(ctx)
	if sid == "" {
		return nil, nil
	}
	events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
	if err != nil {
		return nil, err
	}
	st, err := session.Reduce(events)
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	requestCalls := map[string]bool{}
	text.WriteString(req.System)
	for _, m := range req.Messages {
		text.WriteString(model.Text(m.Content))
		requestCalls[m.ToolCallID] = true
		for _, c := range m.ToolCalls {
			requestCalls[c.GetCallId()] = true
			text.WriteString(c.GetArgumentsJson())
		}
	}
	request := text.String()
	for _, e := range events {
		m := e.GetAssistantMessage()
		if m != nil && st.SupersededRuns[m.GetRunId()] {
			if strings.Contains(request, redoReplyMarker(m.GetRunId())) {
				w.violations = append(w.violations, "superseded assistant content in model request")
			}
			for _, c := range m.GetToolCalls() {
				if id := c.GetCallId(); requestCalls[id] || strings.Contains(request, "[调用 "+id+"]") || strings.Contains(request, "[结果 "+id+"]") || strings.Contains(request, "[失败 "+id+"]") {
					w.violations = append(w.violations, "superseded call or result in model request")
				}
			}
		}
	}
	// 原输入在重答链中逐块相等，只允许保留一份；后续普通 Run 和摘要请求也检查历史 probe。
	probes := map[string]bool{}
	for _, e := range events {
		input := model.Text(e.GetRunRequested().GetInput())
		if strings.HasPrefix(input, redoProbePrefix) {
			probes[input] = true
		}
	}
	for input := range probes {
		if strings.Count(request, input) > 1 {
			w.violations = append(w.violations, "superseded input duplicated in model request")
		}
	}
	return st, nil
}

// redoProbeResponse 由真实 Worker 展示卡片并完成，提供可重答的负载；其他输入继续走原随机脚本。
func (w *World) redoProbeResponse(ctx context.Context, req *model.Request) (*model.Response, error) {
	sid := faults.SessionFrom(ctx)
	if sid == "" {
		return nil, nil
	}
	st, err := w.store.Load(ctx, sid)
	if err != nil {
		return nil, err
	}
	r := st.Active()
	if r == nil || !strings.HasPrefix(model.Text(r.Request.GetInput()), redoProbePrefix) {
		return nil, nil
	}
	resp := &model.Response{Model: "sim/m", Usage: &v1.Usage{InputTokens: uint64(model.EstimateRequest(req)), OutputTokens: 20}}
	if r.Turns == 0 {
		w.Stats.Cards++
		resp.ToolCalls = []*v1.ToolCall{{Capability: card.Capability, ArgumentsJson: `{"card_id":"redo-card","title":"建议","buttons":[{"id":"ok","label":"好"},{"id":"no","label":"不"}]}`}}
	} else {
		resp.Content = model.TextBlocks("活动建议")
	}
	return resp, nil
}
