package runtime

import (
	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/session"
)

// filterModelHistory 是所有模型输入路径共用的重答过滤。替代关系来自完整投影，
// 不从 history 推导：摘要截点可能早于 redo 事件，旧答仍然必须被排除。
// 来源推导、写入闸门与注入判断不调用它，继续使用完整的安全历史。
func filterModelHistory(history []*v1.Event, superseded map[string]bool) []*v1.Event {
	if len(superseded) == 0 {
		return history
	}
	var out []*v1.Event
	for _, e := range history {
		runID := ""
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			runID = p.RunRequested.GetRunId()
		case *v1.Event_Steered:
			runID = p.Steered.GetRunId()
		case *v1.Event_AssistantMessage:
			runID = p.AssistantMessage.GetRunId()
		case *v1.Event_ToolResult:
			runID = p.ToolResult.GetRunId()
		case *v1.Event_MemoryRecalled:
			runID = p.MemoryRecalled.GetRunId()
		}
		if !superseded[runID] {
			out = append(out, e)
		}
	}
	return out
}

func redoView(st *session.State) *session.State {
	if len(st.SupersededRuns) == 0 {
		return st
	}
	v := *st
	v.History = filterModelHistory(st.History, st.SupersededRuns)
	return &v
}
