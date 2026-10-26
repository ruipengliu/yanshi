package sim

import (
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
)

// sim 未开启业务线的健康信息许可（New 中未设置 HealthAllowed）：合法 JSON 里的健康信息也须在审批与执行前拒绝。
// 用固定内容识别注入的样例，不调用 Memory.Check 作判断，避免实现漏检时不变量跟着放过。
// 样例单独作为模型输出时不命中 simModerator 的固定拦截，确保能走到预检查。
const rejectedMemoryArgs = `{"category":"relationship","content":"用户的母亲血压较高"}`

func memoryPrecheckCall(c *v1.ToolCall) bool {
	return c.GetCapability() == "memory_save" && c.GetArgumentsJson() == rejectedMemoryArgs
}

// checkMemoryPrecheck 检查注定被 Memory 预检查拒绝的调用：不请求审批、不开始，有结果时只能是错误。
// 中断或失败可能使调用来不及处理，因此不要求每个请求都有结果。
func checkMemoryPrecheck(sid string, events []*v1.Event) error {
	type key struct{ run, call string }
	bad := map[key]bool{}
	for _, e := range events {
		if m := e.GetAssistantMessage(); m != nil {
			for _, c := range m.GetToolCalls() {
				if memoryPrecheckCall(c) {
					bad[key{m.GetRunId(), c.GetCallId()}] = true
				}
			}
		}
	}
	for _, e := range events {
		switch {
		case bad[key{e.GetApprovalRequested().GetRunId(), e.GetApprovalRequested().GetCallId()}]:
			return fmt.Errorf("invariant: %s requested approval for memory call %s rejected by precheck", sid, e.GetApprovalRequested().GetCallId())
		case bad[key{e.GetToolCallStarted().GetRunId(), e.GetToolCallStarted().GetCallId()}]:
			return fmt.Errorf("invariant: %s started memory call %s rejected by precheck", sid, e.GetToolCallStarted().GetCallId())
		case bad[key{e.GetToolResult().GetRunId(), e.GetToolResult().GetCallId()}] && !e.GetToolResult().GetIsError():
			return fmt.Errorf("invariant: %s memory call %s rejected by precheck has a successful result", sid, e.GetToolResult().GetCallId())
		}
	}
	return nil
}
