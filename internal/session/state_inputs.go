package session

import (
	"fmt"
	"slices"

	"yanshi/internal/capability/capname"
)

// DelegatedInputID 是委托给子 Session 的任务的输入 ID（docs/design/m5-multi-agent.md §4）。
func DelegatedInputID(callID string) string { return "delegate_" + callID }

// delegatedInput 报告 inputID 是否是本 Session 的委托任务。
func (s *State) delegatedInput(inputID string) bool {
	p := s.Created.GetParent()
	return p != nil && inputID != "" && inputID == DelegatedInputID(p.GetCallId())
}

// MaxRecentInputs 是去重窗口：客户端在结果未知时重新提交，通常在几秒内，窗口无需很大；
// 投影随之有界。
const MaxRecentInputs = 64

// InputRef 记录一条带 ID 的输入的结果，重复提交时据此原样返回。
type InputRef struct {
	ID    string
	RunID string
	// Steered 表示并入了进行中的 Run；Answered 非空表示作为对该 ask_user 提问的回答。
	Steered  bool
	Answered string
}

// Input 返回 ID 为 id 的最近输入；不在窗口内时返回 nil。
func (s *State) Input(id string) *InputRef {
	if id == "" {
		return nil
	}
	for i := range s.Inputs {
		if s.Inputs[i].ID == id {
			return &s.Inputs[i]
		}
	}
	return nil
}

// recordInput 记下带 ID 的输入；窗口内重复的 ID 不合法（写入方应先去重）。
func (s *State) recordInput(ref InputRef) error {
	if capname.IsScheduleInput(ref.ID) && s.pendingSchedule == nil {
		return fmt.Errorf("reserved schedule input_id")
	}
	if ref.ID == "" {
		return nil
	}
	if s.Input(ref.ID) != nil {
		return fmt.Errorf("duplicate input id %q", ref.ID)
	}
	s.Inputs = append(s.Inputs, ref)
	if n := len(s.Inputs); n > MaxRecentInputs {
		s.Inputs = slices.Clone(s.Inputs[n-MaxRecentInputs:])
	}
	return nil
}
