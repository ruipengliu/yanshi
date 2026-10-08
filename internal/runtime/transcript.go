package runtime

import (
	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// Transcript 把 Session 历史转换为模型上下文。
//
// 模型接口要求带 tool_calls 的助手消息之后紧跟全部调用结果，因此：
//   - 调用进行中到达的用户输入（Steered 或新 Run）被推迟到结果之后；
//   - 因 Run 中断或失败而永远不会有结果的调用，补一个"未执行"的结果。
func Transcript(st *session.State) []model.Message {
	var out, deferred []model.Message
	var pending []string // 当前助手消息中尚无结果的调用，按顺序

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
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			deferUser(&out, &deferred, pending, p.RunRequested.GetInput())
		case *v1.Event_Steered:
			deferUser(&out, &deferred, pending, p.Steered.GetInput())
		case *v1.Event_AssistantMessage:
			closePending()
			m := p.AssistantMessage
			out = append(out, model.Message{Role: model.RoleAssistant, Content: m.GetContent(), ToolCalls: m.GetToolCalls()})
			for _, tc := range m.GetToolCalls() {
				pending = append(pending, tc.GetCallId())
			}
		case *v1.Event_ToolResult:
			m := p.ToolResult
			content := m.GetContent()
			if m.GetIsError() {
				content = append(model.TextBlocks("[error] "), content...)
			}
			out = append(out, model.Message{Role: model.RoleTool, ToolCallID: m.GetCallId(), Content: content})
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
