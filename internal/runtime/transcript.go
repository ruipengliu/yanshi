package runtime

import (
	"fmt"
	"math"
	"slices"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// summaryPrefix 引出上下文中的压缩摘要。
const summaryPrefix = "[此前的对话已压缩为以下摘要；更早的原文不再可见]\n"

// Transcript 把 Session 历史转换为模型上下文。
//
// 若发生过 Compaction，上下文以最新摘要开头，其后是摘要之后的历史（docs/design/m2-long-runs.md §2）。
// 模型接口要求带 tool_calls 的助手消息之后紧跟全部调用结果，因此：
//   - 调用进行中到达的用户输入（Steered 或新 Run）被推迟到结果之后；
//   - 因 Run 中断或失败而永远不会有结果的调用，补一个"未执行"的结果。
//
// maxToolResult > 0 时，单条调用结果截断到约该 token 数；日志中的原文不受影响。
func Transcript(st *session.State, maxToolResult int) []model.Message {
	return transcript(st, maxToolResult, math.MaxUint64)
}

// transcriptThrough 是 Transcript 中对应 seq ≤ through 的前缀：摘要请求以它为前缀，与主请求共享前缀缓存。
// through 须是闭合边界（session.State.CanCut），此时前缀中没有尚无结果的调用。
func transcriptThrough(st *session.State, maxToolResult int, through uint64) []model.Message {
	return transcript(st, maxToolResult, through)
}

func transcript(st *session.State, maxToolResult int, through uint64) []model.Message {
	var out, deferred []model.Message
	var pending []string // 当前助手消息中尚无结果的调用，按顺序

	if c := st.Compaction; c != nil {
		out = append(out, model.Message{Role: model.RoleUser, Content: append(model.TextBlocks(summaryPrefix), c.GetSummary()...)})
	}
	// 当前 Run 的召回放在它的用户输入之前，而不是系统指令里：系统指令与工具定义保持不变，Run 内只在尾部追加，
	// 前缀缓存才能命中。该输入已被压缩进摘要时，放在摘要之后。
	var recall []*v1.ContentBlock
	active := st.Active()
	if active != nil {
		if t := memoryText(st, active); t != "" {
			recall = model.TextBlocks(t)
		}
	}
	placed := false
	// 当前 Run 的输入不在历史中（已被压缩）时，召回紧跟摘要；这一位置在完整上下文与前缀中相同。
	inHistory := false
	for _, e := range st.History {
		if active != nil && e.GetRunRequested().GetRunId() == active.ID {
			inHistory = true
		}
	}

	closePending := func() {
		for _, id := range pending {
			out = append(out, model.Message{Role: model.RoleTool, ToolCallID: id,
				Content: model.TextBlocks("[error] not executed: the run ended before this call completed")})
		}
		pending = nil
		out = append(out, deferred...)
		deferred = nil
	}

	for _, e := range st.History {
		if e.GetSeq() > through {
			break
		}
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			if recall != nil && p.RunRequested.GetRunId() == active.ID {
				deferUser(&out, &deferred, pending, recall)
				placed = true
			}
			deferUser(&out, &deferred, pending, model.InputBlocks(p.RunRequested.GetInput(), p.RunRequested.GetFromCall()))
		case *v1.Event_Steered:
			deferUser(&out, &deferred, pending, model.InputBlocks(p.Steered.GetInput(), p.Steered.GetFromCall()))
		case *v1.Event_CallTranscript:
			// 双方的话都以用户消息呈现（文本注明说话方）：助手消息只能是模型自己的输出，且不能插在调用与结果之间。
			deferUser(&out, &deferred, pending, model.TextBlocks(model.CallTranscriptText(p.CallTranscript)))
		case *v1.Event_AssistantMessage:
			closePending()
			m := p.AssistantMessage
			out = append(out, model.Message{Role: model.RoleAssistant, Content: m.GetContent(), ToolCalls: m.GetToolCalls()})
			for _, tc := range m.GetToolCalls() {
				pending = append(pending, tc.GetCallId())
			}
		case *v1.Event_ToolResult:
			m := p.ToolResult
			out = append(out, toolMessage(m, maxToolResult))
			for i, id := range pending {
				if id == m.GetCallId() {
					pending = append(pending[:i:i], pending[i+1:]...)
					break
				}
			}
			if len(pending) == 0 {
				out = append(out, deferred...)
				deferred = nil
			}
		}
	}
	closePending()
	if recall != nil && !placed && !inHistory {
		at := 0
		if st.Compaction != nil {
			at = 1
		}
		out = slices.Insert(out, at, model.Message{Role: model.RoleUser, Content: recall})
	}
	return out
}

func deferUser(out, deferred *[]model.Message, pending []string, input []*v1.ContentBlock) {
	m := model.Message{Role: model.RoleUser, Content: input}
	if len(pending) > 0 {
		*deferred = append(*deferred, m)
	} else {
		*out = append(*out, m)
	}
}

func toolMessage(m *v1.ToolResult, maxTokens int) model.Message {
	content := truncateBlocks(m.GetContent(), maxTokens)
	if m.GetIsError() {
		content = append(model.TextBlocks("[error] "), content...)
	}
	return model.Message{Role: model.RoleTool, ToolCallID: m.GetCallId(), Content: content}
}

// truncateBlocks 把内容截断到约 maxTokens 个 token（maxTokens <= 0 表示不限），并注明原始大小。
func truncateBlocks(blocks []*v1.ContentBlock, maxTokens int) []*v1.ContentBlock {
	total := model.EstimateTokens(blocks)
	if maxTokens <= 0 || total <= maxTokens {
		return blocks
	}
	var out []*v1.ContentBlock
	budget := maxTokens
	for _, b := range blocks {
		n := model.EstimateTokens([]*v1.ContentBlock{b})
		if n <= budget {
			out = append(out, b)
			budget -= n
			continue
		}
		if t := b.GetText(); t != nil && budget > 0 {
			head, _ := model.TruncateText(t.GetText(), budget)
			out = append(out, model.TextBlocks(head)...)
		}
		break
	}
	return append(out, model.TextBlocks(fmt.Sprintf("\n[已截断：原始约 %d tokens]", total))...)
}
