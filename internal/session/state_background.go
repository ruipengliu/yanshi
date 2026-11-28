package session

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability/capname"
)

// BackgroundCall 是一个未了结的后台调用。
type BackgroundCall struct {
	CallID string
	// RunID 是发起调用的 Run（转入后台时它已继续推进，多半已经结束）。
	RunID      string
	NodeID     string
	Capability string
	Deadline   time.Time
}

// 后台调用了结的原因（BackgroundResult.reason）。
const (
	BackgroundReasonResult  = "result"
	BackgroundReasonTimeout = "timeout"
)

// OpenBackground 返回未了结的后台调用，按截止时间（相同时按调用 ID）排序。Session 已关闭时返回空：关闭之后不再了结。
func (s *State) OpenBackground() []*BackgroundCall {
	if s.Closed != nil {
		return nil
	}
	out := make([]*BackgroundCall, 0, len(s.Background))
	for _, b := range s.Background {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b *BackgroundCall) int {
		if c := a.Deadline.Compare(b.Deadline); c != 0 {
			return c
		}
		return strings.Compare(a.CallID, b.CallID)
	})
	return out
}

// NextBackgroundDeadline 返回最早的后台截止时间；没有未了结的后台调用时 ok 为 false。
func (s *State) NextBackgroundDeadline() (time.Time, bool) {
	if open := s.OpenBackground(); len(open) > 0 {
		return open[0].Deadline, true
	}
	return time.Time{}, false
}

// BackgroundInput 返回输入中的后台调用了结；不是这种输入时返回 nil。这种输入只由平台写入，且只有这一个内容块。
func BackgroundInput(input []*v1.ContentBlock) *v1.BackgroundResult {
	if len(input) == 1 {
		return input[0].GetBackgroundResult()
	}
	return nil
}

// checkBackgrounded 校验调用转入后台（ADR-0035、ADR-0058）：只有 Worker 能把已派发、尚无结果的设备调用或委托调用转入
// 后台。平台保留的 Node ID 只有委托能转入后台，且截止时间不能晚于原截止时间（转后台只改变父 Run
// 是否等待，不延长子任务的时限）。委托派生的子 Session 不转后台：它的委托 Run 产出回复，转入后台的调用迟到的结果会开启
// 一个没有父调用在等的新 Run。
func (s *State) checkBackgrounded(m *v1.ToolResult, c *Call) error {
	b := m.GetBackgrounded()
	switch {
	case m.GetAttempt() == 0:
		return fmt.Errorf("external result cannot background call %q", m.GetCallId())
	case !c.Dispatched() || b.GetNodeId() != c.NodeID:
		return fmt.Errorf("call %q is not dispatched to node %q", m.GetCallId(), b.GetNodeId())
	case capname.IsReservedNode(c.NodeID) && !capname.IsDelegationNode(c.NodeID):
		return fmt.Errorf("call %q to %s cannot run in the background", m.GetCallId(), c.NodeID)
	case capname.IsDelegationNode(c.NodeID) && b.GetDeadline() != nil && b.GetDeadline().AsTime().After(c.Deadline):
		return fmt.Errorf("background delegation %q cannot extend its deadline", m.GetCallId())
	case s.Created.GetParent() != nil:
		return fmt.Errorf("call %q: a delegated session does not background calls", m.GetCallId())
	case b.GetDeadline() == nil:
		return fmt.Errorf("background call %q without a deadline", m.GetCallId())
	}
	return nil
}

// checkBackgroundInput 校验输入中的后台调用了结：只能是输入中唯一的内容块，针对未了结的后台调用，没有输入 ID、不来自 Call；
// 其中设备返回的内容不能含平台才写入的内容块（卡片、卡片动作、界面上下文、嵌套的了结）。不含它的输入返回 nil。
func (s *State) checkBackgroundInput(input []*v1.ContentBlock, inputID, fromCall string) (*v1.BackgroundResult, error) {
	n := 0
	for _, b := range input {
		if b.GetBackgroundResult() != nil {
			n++
		}
	}
	if n == 0 {
		return nil, nil
	}
	bg := BackgroundInput(input)
	if bg == nil {
		return nil, errors.New("a background result must be the only block of its input")
	}
	if inputID != "" || fromCall != "" {
		return nil, errors.New("background result with an input id or from a call")
	}
	b := s.Background[bg.GetCallId()]
	if b == nil || s.Closed != nil {
		return nil, fmt.Errorf("call %q is not an open background call", bg.GetCallId())
	}
	if bg.GetCapability() != b.Capability {
		return nil, fmt.Errorf("background call %q is %s, not %s", bg.GetCallId(), b.Capability, bg.GetCapability())
	}
	switch bg.GetReason() {
	case BackgroundReasonResult:
	case BackgroundReasonTimeout:
		if len(bg.GetContent()) > 0 {
			return nil, fmt.Errorf("background call %q timed out with content", bg.GetCallId())
		}
	default:
		return nil, fmt.Errorf("background result reason %q", bg.GetReason())
	}
	for _, c := range bg.GetContent() {
		if c.GetCard() != nil || c.GetCardAction() != nil || c.GetUiContext() != nil || c.GetBackgroundResult() != nil || c.GetCapture() != nil {
			return nil, fmt.Errorf("background result of call %q carries a platform block", bg.GetCallId())
		}
	}
	return bg, nil
}

// settleBackground 了结后台调用：每个后台调用至多了结一次（结果、到期、取消三者之一）。
func (s *State) settleBackground(callID string) {
	delete(s.Background, callID)
}
