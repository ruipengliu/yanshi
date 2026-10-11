package session

import v1 "yanshi/gen/yanshi/v1"

// ExternalResult 是由平台写入的、不属于任何 Attempt 的调用结果（attempt = 0）：设备结果、委托结果、用户的回答。
// attempt = 0 的结果不受 Attempt fencing：写入方不是 Worker，结果在 Run 被接管前后都有效。
func ExternalResult(runID, callID string, content []*v1.ContentBlock, isErr bool) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{
		RunId: runID, CallId: callID, Content: content, IsError: isErr,
	}}}
}
