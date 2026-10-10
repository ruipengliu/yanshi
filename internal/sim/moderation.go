package sim

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/runtime"
)

// 内容安全的模拟（docs/design/m4-moderation.md §6）：模拟提供商按文本的散列决定是否拦截——同一文本总得到
// 同一结论，因此"被拦截的输出从不出现在日志中"可以精确检查；故障模式下还随机返回错误（服务不可用）。

type simModerator struct{ w *World }

// blocked 按散列拦截约 1/12 的文本（长 Run 模式下约 1/400：被拦截的输出会使 Run 以拒答结束，
// 拦截太多则长 Run 几乎跑不满）；拒答文本本身不拦截。
func (w *World) blocked(text string) bool {
	if text == moderation.Refusal {
		return false
	}
	h := fnv.New32a()
	h.Write([]byte(text))
	if w.opts.LongRuns {
		return h.Sum32()%400 == 0
	}
	return h.Sum32()%12 == 0
}

func (m simModerator) Check(_ context.Context, req moderation.Request) (moderation.Verdict, error) {
	if m.w.faults && m.w.chance(0.03) {
		m.w.Stats.ModerationErrors++
		return moderation.Verdict{}, errors.New("simulated moderation outage")
	}
	if !m.w.blocked(req.Text) {
		return moderation.Verdict{}, nil
	}
	if req.Stage == moderation.Output {
		m.w.Stats.OutputBlocks++
	}
	return moderation.Verdict{Block: true, Labels: []string{"sim"}}, nil
}

// submitModerated 报告提交是否因内容安全被拒绝或无法检查（预期结果，不是错误）。
func (w *World) submitModerated(err error) bool {
	switch {
	case errors.Is(err, moderation.ErrRejected):
		w.Stats.InputBlocks++
		return true
	case errors.Is(err, moderation.ErrUnavailable):
		return true
	}
	return false
}

// checkModeration 检查：日志中没有会被拦截的输入或输出；每个 ContentModerated 之后紧跟拒答文本且没有调用。
func (w *World) checkModeration() error {
	ctx := context.Background()
	for _, sid := range append(slices.Clone(w.sessions), w.closed...) {
		events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
		if err != nil {
			return err
		}
		for i, e := range events {
			switch p := e.GetPayload().(type) {
			case *v1.Event_RunRequested:
				if w.blocked(model.Text(p.RunRequested.GetInput())) {
					return fmt.Errorf("invariant: blocked input reached the log of %s at seq %d", sid, e.GetSeq())
				}
			case *v1.Event_Steered:
				if w.blocked(model.Text(p.Steered.GetInput())) {
					return fmt.Errorf("invariant: blocked steer reached the log of %s at seq %d", sid, e.GetSeq())
				}
			case *v1.Event_AssistantMessage:
				if w.blocked(runtime.ModerationText(p.AssistantMessage.GetContent(), p.AssistantMessage.GetToolCalls())) {
					return fmt.Errorf("invariant: blocked output reached the log of %s at seq %d", sid, e.GetSeq())
				}
			case *v1.Event_ContentModerated:
				var next *v1.AssistantMessage
				if i+1 < len(events) {
					next = events[i+1].GetAssistantMessage()
				}
				if next == nil || model.Text(next.GetContent()) != moderation.Refusal || len(next.GetToolCalls()) > 0 {
					return fmt.Errorf("invariant: ContentModerated in %s at seq %d is not followed by the refusal", sid, e.GetSeq())
				}
			}
		}
	}
	return nil
}
