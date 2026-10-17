package nodesdk

import (
	"slices"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

// BackgroundCall 是一个未了结的后台调用（ADR-0035）：设备离线超过宽限期，Run 不再等它；设备上线后执行，结果作为一条
// 输入送达，到 Deadline 仍未执行则撤回。
type BackgroundCall struct {
	CallID     string
	Capability string
	NodeID     string
	// RunID 是发起调用的 Run。
	RunID    string
	Deadline time.Time
}

// BackgroundCalls 从一个 Session 的事件里跟踪未了结的后台调用（与网页 SDK 的 BackgroundCalls 同义）：
//   - 带 backgrounded 的 ToolResult：调用转入后台；
//   - 输入（RunRequested、Steered）中的 background_result：设备结果或到期，了结；
//   - BackgroundCancelled：取消，了结。
//
// 喂给它的事件要从 seq 0 开始，否则看不到续传位置之前转入后台的调用。不是并发安全的：在 SessionHandler 中调用。
type BackgroundCalls struct {
	// capabilities 是尚无结果的调用的工具名，转入后台时取用；结果到达后移除，因此有界。
	capabilities map[string]string
	open         map[string]BackgroundCall
}

// OnEvent 依据一个事件更新状态。
func (b *BackgroundCalls) OnEvent(e *v1.Event) {
	if b.open == nil {
		b.open, b.capabilities = map[string]BackgroundCall{}, map[string]string{}
	}
	for _, tc := range e.GetAssistantMessage().GetToolCalls() {
		b.capabilities[tc.GetCallId()] = tc.GetCapability()
	}
	if r := e.GetToolResult(); r != nil {
		if bg := r.GetBackgrounded(); bg != nil {
			b.open[r.GetCallId()] = BackgroundCall{CallID: r.GetCallId(), Capability: b.capabilities[r.GetCallId()],
				NodeID: bg.GetNodeId(), RunID: r.GetRunId(), Deadline: bg.GetDeadline().AsTime()}
		}
		delete(b.capabilities, r.GetCallId())
	}
	if r := BackgroundResultOf(e); r != nil {
		delete(b.open, r.GetCallId())
	}
	if c := e.GetBackgroundCancelled(); c != nil {
		delete(b.open, c.GetCallId())
	}
}

// Items 返回未了结的后台调用，按截止时间排序。
func (b *BackgroundCalls) Items() []BackgroundCall {
	out := make([]BackgroundCall, 0, len(b.open))
	for _, c := range b.open {
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y BackgroundCall) int {
		if c := x.Deadline.Compare(y.Deadline); c != 0 {
			return c
		}
		return strings.Compare(x.CallID, y.CallID)
	})
	return out
}

// BackgroundResultOf 返回事件中的后台调用了结（设备结果或到期，ADR-0035）；不是这种事件时返回 nil。它是平台写入的输入，
// 客户端应把它显示为"设备返回了结果"一类的说明，而不是用户说的话。
func BackgroundResultOf(e *v1.Event) *v1.BackgroundResult {
	input := e.GetRunRequested().GetInput()
	if input == nil {
		input = e.GetSteered().GetInput()
	}
	if len(input) == 1 {
		return input[0].GetBackgroundResult()
	}
	return nil
}
