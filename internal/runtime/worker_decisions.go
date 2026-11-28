package runtime

import (
	"context"
	"errors"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/decision"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// maxJudgedPerStep 限制一步中判断的调用结果数，使一步的时长有界。
const maxJudgedPerStep = 8

// unjudged 返回本 Run 中来自外部、尚未判断的调用结果。外部指设备、沙箱、委托（路由调用）与 MCP 工具：它们的内容
// 不由平台产生（工具名形如 "<来源>__<能力>"）；用户对提问的回答不算。并入本 Run 的后台调用迟到的结果也算（ADR-0035），
// 含有他人发言的旁听转写也算（ADR-0036）；平台定时不属于外部内容，不进入此判断。来源由平台推导（session.Provenance，ADR-0039）。
func (w *Worker) unjudged(r *session.Run) []session.Piece {
	var out []session.Piece
	for _, p := range w.st.Pieces() {
		res := p.Result
		if res == nil || res.GetRunId() != r.ID || (p.From != session.FromExternal && p.From != session.FromOther) {
			continue
		}
		if _, done := r.Decisions[session.DecisionKey(decision.ToolResultInjection, res.GetCallId())]; done {
			continue
		}
		if out = append(out, p); len(out) == maxJudgedPerStep {
			break
		}
	}
	return out
}

// judgeStepBudget 是一步中全部判断的总时限：预算用完后，其余结果直接记为不可用（默认路径），一步的时长因此有界，
// 与每步判断几条无关。
const judgeStepBudget = 4 * time.Second

// judgeResults 判断调用结果是否含注入，记为影子模式的 DecisionMade（只记录、不影响执行）。判断不可用时同样记录，
// 选项为空（走默认路径），不在下一步重试。判断与模型调用一样在 during 中进行：期间续约，本 Attempt 被接管或
// 中断时作废（评审 §11 问题 5）。
func (w *Worker) judgeResults(ctx context.Context, r *session.Run, results []session.Piece) error {
	var events []*v1.Event
	err := w.during(ctx, r, func(ctx context.Context) error {
		bctx, cancel := context.WithTimeout(ctx, judgeStepBudget)
		defer cancel()
		for _, p := range results {
			// 旁听转写只判断他人的话（Piece.Text）：佩戴者的话是用户本人的输入，平台写的说明也不是外部内容。
			q := decision.Question{Kind: decision.ToolResultInjection, Subject: p.Result.GetCallId(),
				Text: p.Text, Options: decision.Options(decision.ToolResultInjection)}
			if t, cut := model.Truncate(q.Text, 8000); cut {
				q.Text = t
			}
			a, err := w.cfg.InjectionCheck.Decide(bctx, q)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 用量与判断是否可用无关：调用了模型就计量（ADR-0019），记在被调用的模型名下。
			if a.Usage != nil {
				w.cfg.Meter.Model(ctx, w.usageScope(), "use_"+w.cfg.Store.IDs(), agentdef.Label(w.st.Agent), a.Billed, a.Usage, w.now())
			}
			if err != nil {
				a = decision.Answer{Model: "unavailable"}
			}
			events = append(events, &v1.Event{Payload: &v1.Event_DecisionMade{DecisionMade: &v1.DecisionMade{
				RunId: r.ID, Attempt: w.attempt, Kind: q.Kind, Subject: q.Subject, Options: q.Options,
				Chosen: a.Chosen, Confidence: float32(a.Confidence), Model: a.Model, Shadow: true}}})
		}
		return nil
	})
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errSuperseded):
		return nil
	case err != nil:
		return err
	}
	return w.commit(ctx, events...)
}
