package service

import (
	"context"
	"errors"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability/capname"
	"yanshi/internal/card"
	"yanshi/internal/lifecycle"
	"yanshi/internal/session"
)

const (
	inputStatusScanLimit = 4096
	inputStatusPageSize  = 64
)

var ErrScanBudgetExceeded = errors.New("input status scan budget exceeded")

type InputStatus struct {
	Found           bool
	AsOfSeq         uint64
	InputID         string
	RunID           string
	EventID         string
	AcceptedSeq     uint64
	AcceptanceKind  string
	AnsweredCallID  string
	RunStatus       session.RunStatus
	AttemptCount    uint32
	TakeoverCount   uint32
	Calls           InputCallEvidence
	SinceAcceptance *InputAcceptanceEvidence
}

type InputCallEvidence struct {
	WritesWithoutResult      int `json:"writes_without_result"`
	WritesWithError          int `json:"writes_with_error"`
	WritesBackgrounded       int `json:"writes_backgrounded"`
	ResumesWithPendingWrites int `json:"resumes_with_pending_writes"`
}

type InputAcceptanceEvidence struct {
	AttemptsWithoutPriorSuspend uint32            `json:"attempts_without_prior_suspend"`
	Calls                       InputCallEvidence `json:"calls"`
}

// InputStatus 以授权时的投影序号固定读取范围，使命中和未命中都对应同一份日志。
func (s *Service) InputStatus(ctx context.Context, sessionID, inputID string) (InputStatus, error) {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return InputStatus{}, err
	}
	if len(inputID) == 0 || len(inputID) > MaxInputIDLen {
		return InputStatus{}, fmt.Errorf("%w: input_id must be 1 to %d bytes", ErrInvalid, MaxInputIDLen)
	}
	status, scanErr := s.scanInputStatus(ctx, st, inputID)
	// 扫描期间清理可能已经移走日志，删除优先于任何扫描结果（包括冲突和读到一半的错误）。
	if s.Deletions != nil {
		deleted, err := lifecycle.Deleted(ctx, s.Deletions, sessionID)
		if err != nil {
			return InputStatus{}, err
		}
		if deleted {
			return InputStatus{}, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
		}
	}
	return status, scanErr
}

func (s *Service) scanInputStatus(ctx context.Context, st *session.State, inputID string) (InputStatus, error) {
	out := InputStatus{AsOfSeq: st.Seq}
	parent := st.Created.GetParent()
	if capname.IsScheduleInput(inputID) || (parent != nil && inputID == session.DelegatedInputID(parent.GetCallId())) {
		return out, nil
	}
	if st.Seq > inputStatusScanLimit {
		return InputStatus{}, ErrScanBudgetExceeded
	}
	// 一个 Session 同时只有一个活跃 Run。只保留当前 Run 和命中 Run 的证据。
	var current, matched, sinceAcceptance *inputEvidenceScan
	for after := uint64(0); after < st.Seq; {
		events, err := s.Store.Log.Read(ctx, st.SessionID, after, int(min(inputStatusPageSize, st.Seq-after)))
		if err != nil {
			return InputStatus{}, err
		}
		if len(events) == 0 {
			return InputStatus{}, fmt.Errorf("input status scan ended before seq %d", st.Seq)
		}
		for _, e := range events {
			if e.GetSeq() != after+1 || e.GetSeq() > st.Seq {
				return InputStatus{}, fmt.Errorf("input status scan received an invalid sequence after %d", after)
			}
			after = e.GetSeq()
			if e.GetRunRequested() != nil {
				current = &inputEvidenceScan{}
			}
			if runID, kind, callID := inputAcceptance(e, inputID); kind != "" {
				if out.Found {
					return InputStatus{}, &ReasonError{Err: ErrConflict, Reason: "input_accepted_more_than_once"}
				}
				out.Found, out.InputID, out.RunID = true, inputID, runID
				out.EventID, out.AcceptedSeq = e.GetId(), e.GetSeq()
				out.AcceptanceKind, out.AnsweredCallID = kind, callID
				matched = current
				// 插话以前的调用仍属于整 Run，但旧客户端从接受位置才开始归约。
				if kind == "steered" {
					sinceAcceptance = &inputEvidenceScan{}
				}
			}
			if current != nil {
				current.observe(e)
				if current == matched && sinceAcceptance != nil {
					sinceAcceptance.observe(e)
				}
			}
		}
	}
	if !out.Found {
		return out, nil
	}
	run, ok := st.RunSummary(out.RunID)
	if !ok || matched == nil {
		return InputStatus{}, fmt.Errorf("input status scan found no Run %s", out.RunID)
	}
	out.RunStatus, out.AttemptCount = run.Status, run.Attempt
	out.TakeoverCount, out.Calls = matched.facts.AttemptsWithoutPriorSuspend, matched.facts.Calls
	if sinceAcceptance != nil {
		out.SinceAcceptance = &sinceAcceptance.facts
	}
	return out, nil
}

func inputAcceptance(e *v1.Event, id string) (runID, kind, callID string) {
	switch p := e.GetPayload().(type) {
	case *v1.Event_RunRequested:
		m := p.RunRequested
		if m.GetInputId() == id && m.GetFromCall() == "" && !derivedCardInput(id, m.GetInput()) {
			return m.GetRunId(), "requested", ""
		}
	case *v1.Event_Steered:
		m := p.Steered
		if m.GetInputId() == id && m.GetFromCall() == "" && !derivedCardInput(id, m.GetInput()) {
			return m.GetRunId(), "steered", ""
		}
	case *v1.Event_ToolResult:
		m := p.ToolResult
		if m.GetInputId() == id && m.GetFromCall() == "" && m.GetCardAction() == nil && !derivedCardInput(id, m.GetContent()) {
			return m.GetRunId(), "answered", m.GetCallId()
		}
	}
	return "", "", ""
}

func derivedCardInput(id string, input []*v1.ContentBlock) bool {
	for _, block := range input {
		if action := block.GetCardAction(); action != nil && card.InputID(action) == id {
			return true
		}
	}
	return false
}

type inputEvidenceScan struct {
	facts     InputAcceptanceEvidence
	pending   map[string]struct{}
	suspended bool
}

func (s *inputEvidenceScan) observe(e *v1.Event) {
	switch p := e.GetPayload().(type) {
	case *v1.Event_RunSuspended:
		s.suspended = true
	case *v1.Event_AttemptStarted:
		// Run 内首次 Attempt 从 queued 开始，后续 Attempt 要么接管，要么从挂起恢复。
		if p.AttemptStarted.GetAttempt() > 1 {
			if !s.suspended {
				s.facts.AttemptsWithoutPriorSuspend++
			} else if len(s.pending) > 0 {
				s.facts.Calls.ResumesWithPendingWrites++
			}
		}
		s.suspended = false
	case *v1.Event_ToolCallStarted:
		m := p.ToolCallStarted
		if !m.GetReadOnly() {
			if s.pending == nil {
				s.pending = map[string]struct{}{}
			}
			s.pending[m.GetCallId()] = struct{}{}
			s.facts.Calls.WritesWithoutResult = len(s.pending)
		}
	case *v1.Event_ToolResult:
		m := p.ToolResult
		if _, ok := s.pending[m.GetCallId()]; ok {
			delete(s.pending, m.GetCallId())
			s.facts.Calls.WritesWithoutResult = len(s.pending)
			if m.GetIsError() {
				s.facts.Calls.WritesWithError++
			}
			if m.GetBackgrounded() != nil {
				s.facts.Calls.WritesBackgrounded++
			}
		}
	}
}
