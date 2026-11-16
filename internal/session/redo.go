package session

import (
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
)

// RedoBlocker 只按日志投影判断最后一个 Run 的重答资格，返回不含内容的原因类别。
// service 的每次决策与 Apply 共用它，避免冲突重算或直接写事件绕过资格。
func (s *State) RedoBlocker(runID string) string {
	if s.Closed != nil {
		return "session_closed"
	}
	if len(s.Runs) == 0 || runID == "" || s.Runs[len(s.Runs)-1].ID != runID {
		return "not_last_run"
	}
	r := s.Runs[len(s.Runs)-1]
	if r.Status != RunCompleted {
		return "not_completed"
	}
	if s.Created.GetParent() != nil {
		return "delegated_session"
	}
	// TODO：三方入站 Session 尚无入口；接入时按结构化来源拒绝重答。
	if r.Request == nil || r.RequestedSeq == 0 || r.Request.GetFromCall() != "" || r.Request.GetSchedule() != nil || s.delegatedInput(r.Request.GetInputId()) {
		return "platform_input"
	}
	for _, b := range r.Request.GetInput() {
		if b.GetCardAction() != nil {
			return "card_action_input"
		}
		if b.GetBackgroundResult() != nil || b.GetCapture() != nil || b.GetCard() != nil {
			return "platform_input"
		}
	}
	if s.CompactedAt > r.RequestedSeq {
		return "compacted"
	}
	if r.Steered {
		return "steered"
	}
	for _, c := range r.Calls {
		if c.Approval != nil {
			return "approval_requested"
		}
		if c.Call.GetCapability() == askuser.Capability || c.NodeID == askuser.NodeID {
			return "ask_user"
		}
		if c.IsDelegation() {
			return "delegation"
		}
		if c.Background {
			return "background_call"
		}
		// show_card 是唯一豁免，不能把低风险或幂等当成只读证据。
		if c.Call.GetCapability() != ShowCardCapability && !(&Run{Calls: []*Call{c}}).CallsReadOnly() {
			return "not_read_only"
		}
	}
	return ""
}

func (s *State) checkRedo(m *v1.RunRequested) error {
	if m.GetRedoOf() == "" {
		return nil
	}
	if reason := s.RedoBlocker(m.GetRedoOf()); reason != "" {
		return fmt.Errorf("redo: %s", reason)
	}
	if m.GetSchedule() != nil || m.GetFromCall() != "" || s.delegatedInput(m.GetInputId()) {
		return fmt.Errorf("redo: platform_input")
	}
	want := s.Run(m.GetRedoOf()).Request.GetInput()
	if !slices.EqualFunc(m.GetInput(), want, func(a, b *v1.ContentBlock) bool { return proto.Equal(a, b) }) {
		return fmt.Errorf("redo: input_changed")
	}
	return nil
}

// supersede 只在校验全部通过后调用；旧 map 不再修改，Check 的副本不能污染原投影。
func (s *State) supersede(runID string) {
	if runID == "" {
		return
	}
	s.SupersededRuns = maps.Clone(s.SupersededRuns)
	if s.SupersededRuns == nil {
		s.SupersededRuns = map[string]bool{}
	}
	s.SupersededRuns[runID] = true
	s.SupersededCardCalls = maps.Clone(s.SupersededCardCalls)
	// 资格已排除输入之后的压缩，目标 Run 的全部卡片结果仍在 History 中。
	for _, e := range s.History {
		m := e.GetToolResult()
		if m.GetRunId() == runID && hasCard(m.GetContent()) {
			if s.SupersededCardCalls == nil {
				s.SupersededCardCalls = map[string]bool{}
			}
			s.SupersededCardCalls[m.GetCallId()] = true
		}
	}
}

func hasCardAction(blocks []*v1.ContentBlock) bool {
	return slices.ContainsFunc(blocks, func(b *v1.ContentBlock) bool { return b.GetCardAction() != nil })
}

func (s *State) checkCardAction(a *v1.CardAction) error {
	if a != nil && s.SupersededCardCalls[a.GetCallId()] {
		return fmt.Errorf("card action: superseded_call")
	}
	return nil
}

func (s *State) checkCardActions(blocks []*v1.ContentBlock) error {
	for _, b := range blocks {
		if err := s.checkCardAction(b.GetCardAction()); err != nil {
			return err
		}
	}
	return nil
}
