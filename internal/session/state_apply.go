package session

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/capability/capname"
	"yanshi/internal/shard"
)

func (s *State) apply(e *v1.Event) error {
	if e.GetSeq() != s.Seq+1 {
		return fmt.Errorf("seq gap: want %d", s.Seq+1)
	}
	if s.Created == nil {
		c := e.GetSessionCreated()
		if c == nil {
			return fmt.Errorf("first event must be SessionCreated, got %T", e.GetPayload())
		}

		s.SessionID, s.Created, s.Agent, s.Seq = e.GetSessionId(), c, c.GetAgent(), e.GetSeq()
		return nil
	}
	if e.GetSessionId() != s.SessionID {
		return fmt.Errorf("event belongs to session %q", e.GetSessionId())
	}
	if s.Closed != nil && e.GetScheduleSkipped() == nil {
		return fmt.Errorf("session is closed")
	}

	if s.pendingSchedule != nil && e.GetRunRequested() == nil {
		return fmt.Errorf("ScheduleFired must be followed by RunRequested")
	}
	switch p := e.GetPayload().(type) {
	case *v1.Event_ScheduleCreated, *v1.Event_ScheduleCancelled, *v1.Event_ScheduleFired, *v1.Event_ScheduleSkipped:
		if err := s.applySchedule(e); err != nil {
			return err
		}
	case *v1.Event_SessionCreated:
		return fmt.Errorf("duplicate SessionCreated")

	case *v1.Event_ArtifactCopied:
		// 委托交接的复制记录（ADR-0055）：只是审计，不改变投影；副本须落在本 Session 的槽位上，来源是另一个 Session。
		if err := checkArtifactCopied(s.SessionID, p.ArtifactCopied); err != nil {
			return err
		}

	case *v1.Event_SessionClosed:
		// 关闭方须先中断活跃的 Run、结束进行中的 Call（可与本事件同批提交）。
		if a := s.Active(); a != nil {
			return fmt.Errorf("session closed while run %s is active", a.ID)
		}
		if s.ActiveCall != nil {
			return fmt.Errorf("session closed while call %s is active", s.ActiveCall.ID)
		}
		s.Closed = p.SessionClosed

	case *v1.Event_CallStarted:
		m := p.CallStarted
		// 新 Call 开始前，写入方须先结束旧的（"replaced"，可同批提交）。
		if s.ActiveCall != nil {
			return fmt.Errorf("call %s started while call %s is active", m.GetCallId(), s.ActiveCall.ID)
		}
		if m.GetCallId() == "" {
			return fmt.Errorf("call started without an id")
		}
		s.ActiveCall = &ActiveCall{ID: m.GetCallId(), DeviceID: m.GetDeviceId(), StartedAt: e.GetTime().AsTime()}

	case *v1.Event_CallTranscript:
		m := p.CallTranscript
		if err := s.checkCall(m.GetCallId()); err != nil {
			return err
		}
		if !slices.Contains(CallRoles, m.GetRole()) || m.GetText() == "" {
			return fmt.Errorf("invalid call transcript (role %q, %d bytes)", m.GetRole(), len(m.GetText()))
		}
		s.History = append(s.History, e)

	case *v1.Event_CallEnded:
		if err := s.checkCall(p.CallEnded.GetCallId()); err != nil {
			return err
		}
		s.ActiveCall = nil

	case *v1.Event_ContentModerated:
		// 只校验归属：拦截不改变状态，替换后的 AssistantMessage 随后照常应用。
		if _, err := s.fencedRun(p.ContentModerated.GetRunId(), p.ContentModerated.GetAttempt()); err != nil {
			return err
		}

	case *v1.Event_AgentSwitched:
		m := p.AgentSwitched
		if a := s.Active(); a != nil {
			return fmt.Errorf("agent switched while run %s is active", a.ID)
		}
		if !proto.Equal(m.GetFrom(), s.Agent) || m.GetTo().GetName() != s.Agent.GetName() || m.GetTo().GetVersion() == "" {
			return fmt.Errorf("invalid agent switch %v → %v from %v", m.GetFrom(), m.GetTo(), s.Agent)
		}
		s.Agent = m.GetTo()

	case *v1.Event_RunRequested:
		if err := s.checkRedo(p.RunRequested); err != nil {
			return err
		}
		if err := s.checkCardActions(p.RunRequested.GetInput()); err != nil {
			return err
		}
		if err := s.checkScheduleInput(e); err != nil {
			return err
		}
		if err := s.checkFromCall(p.RunRequested.GetFromCall()); err != nil {
			return err
		}
		if hasCard(p.RunRequested.GetInput()) {
			return errors.New("card in user input")
		}
		if hasCapture(p.RunRequested.GetInput()) {
			return errors.New("capture segment in user input")
		}
		bg, err := s.checkBackgroundInput(p.RunRequested.GetInput(), p.RunRequested.GetInputId(), p.RunRequested.GetFromCall())
		if err != nil {
			return err
		}
		if a := s.Active(); a != nil {
			return fmt.Errorf("run %s requested while run %s is active", p.RunRequested.GetRunId(), a.ID)
		}
		if _, known := s.RunSummary(p.RunRequested.GetRunId()); p.RunRequested.GetRunId() == "" || known {
			return fmt.Errorf("invalid or duplicate run id %q", p.RunRequested.GetRunId())
		}
		if err := s.recordInput(InputRef{ID: p.RunRequested.GetInputId(), RunID: p.RunRequested.GetRunId()}); err != nil {
			return err
		}
		if s.Delegated == "" && s.delegatedInput(p.RunRequested.GetInputId()) {
			s.Delegated = p.RunRequested.GetRunId()
		}
		s.supersede(p.RunRequested.GetRedoOf())
		run := &Run{ID: p.RunRequested.GetRunId(), Status: RunQueued, RequestedAt: e.GetTime().AsTime(),
			Request: p.RunRequested, RequestedSeq: e.GetSeq()}
		if bg != nil {
			run.FromBackground = bg.GetCallId()
			s.settleBackground(bg.GetCallId())
		}
		s.pendingSchedule = nil
		s.Runs = append(s.Runs, run)
		s.trimRuns()
		s.History = append(s.History, e)

	case *v1.Event_Steered:
		if err := s.checkCardActions(p.Steered.GetInput()); err != nil {
			return err
		}
		if _, err := s.activeRun(p.Steered.GetRunId()); err != nil {
			return err
		}
		if err := s.checkFromCall(p.Steered.GetFromCall()); err != nil {
			return err
		}
		if hasCard(p.Steered.GetInput()) {
			return errors.New("card in user input")
		}
		if hasCapture(p.Steered.GetInput()) {
			return errors.New("capture segment in user input")
		}
		bg, err := s.checkBackgroundInput(p.Steered.GetInput(), p.Steered.GetInputId(), p.Steered.GetFromCall())
		if err != nil {
			return err
		}
		if err := s.recordInput(InputRef{ID: p.Steered.GetInputId(), RunID: p.Steered.GetRunId(), Steered: true}); err != nil {
			return err
		}
		if bg != nil {
			s.settleBackground(bg.GetCallId())
		}
		if s.Delegated == "" && s.delegatedInput(p.Steered.GetInputId()) {
			s.Delegated = p.Steered.GetRunId()
		}
		s.Run(p.Steered.GetRunId()).Steered = true
		s.History = append(s.History, e)

	case *v1.Event_AttemptStarted:
		r, err := s.activeRun(p.AttemptStarted.GetRunId())
		if err != nil {
			return err
		}
		if p.AttemptStarted.GetAttempt() != r.Attempt+1 {
			return fmt.Errorf("attempt %d does not follow %d", p.AttemptStarted.GetAttempt(), r.Attempt)
		}
		if r.Status == RunRunning {
			r.Takeovers++
			r.StalledTakeovers++
		}
		r.Attempt, r.Status, r.LiveEndpoint = p.AttemptStarted.GetAttempt(), RunRunning, p.AttemptStarted.GetLiveEndpoint()
		r.SuspendReason, r.SuspendedUntil, r.Resume = "", time.Time{}, false

	case *v1.Event_RunSuspended:
		if _, err := s.fencedRun(p.RunSuspended.GetRunId(), p.RunSuspended.GetAttempt()); err != nil {
			return err
		}
		r := s.Run(p.RunSuspended.GetRunId())
		r.Status, r.SuspendReason = RunSuspended, p.RunSuspended.GetReason()
		r.SuspendedUntil = time.Time{}
		if u := p.RunSuspended.GetUntil(); u != nil {
			r.SuspendedUntil = u.AsTime()
		}
		if r.SuspendReason == SuspendModeration {
			if r.ModerationWaits == 0 {
				r.ModerationWaitSince = e.GetTime().AsTime()
			}
			r.ModerationWaits++
		}

	case *v1.Event_ApprovalRequested:
		m := p.ApprovalRequested
		r, err := s.fencedRun(m.GetRunId(), m.GetAttempt())
		if err != nil {
			return err
		}
		c := r.call(m.GetCallId())
		if c == nil || c.Done || c.Started() || c.Approval != nil {
			return fmt.Errorf("call %q cannot request approval", m.GetCallId())
		}
		c.Approval = &Approval{Summary: m.GetSummary(), Deadline: m.GetDeadline().AsTime()}

	case *v1.Event_ApprovalDecided:
		m := p.ApprovalDecided
		r, err := s.activeRun(m.GetRunId())
		if err != nil {
			return err
		}
		c := r.call(m.GetCallId())
		if c == nil || c.Done || !c.AwaitingApproval() {
			return fmt.Errorf("call %q is not awaiting approval", m.GetCallId())
		}
		c.Approval.Decided, c.Approval.Approved = true, m.GetApproved()
		r.Resume = true

	case *v1.Event_AssistantMessage:
		m := p.AssistantMessage
		r, err := s.fencedRun(m.GetRunId(), m.GetAttempt())
		if err != nil {
			return err
		}
		if c := r.PendingCall(); c != nil {
			return fmt.Errorf("assistant message while call %s is pending", c.Call.GetCallId())
		}
		if len(m.GetLoopState()) > MaxLoopState {
			return fmt.Errorf("loop state of %d bytes exceeds %d", len(m.GetLoopState()), MaxLoopState)
		}
		seen := map[string]bool{}
		for _, tc := range m.GetToolCalls() {
			if tc.GetCallId() == "" || seen[tc.GetCallId()] || s.hasCallID(tc.GetCallId()) {
				return fmt.Errorf("invalid or duplicate call id %q", tc.GetCallId())
			}
			seen[tc.GetCallId()] = true
		}
		for _, tc := range m.GetToolCalls() {
			r.Calls = append(r.Calls, &Call{Call: tc})
			s.addCallID(tc.GetCallId())
		}
		r.Turns++
		r.LoopState, r.LoopStateVersion = m.GetLoopState(), m.GetLoopStateVersion()
		r.OutputTokens += int64(m.GetUsage().GetOutputTokens())
		r.StalledTakeovers = 0
		r.ModerationWaits, r.ModerationWaitSince = 0, time.Time{}
		s.History = append(s.History, e)

	case *v1.Event_ToolCallStarted:
		m := p.ToolCallStarted
		r, err := s.fencedRun(m.GetRunId(), m.GetAttempt())
		if err != nil {
			return err
		}
		c := r.call(m.GetCallId())
		if c == nil || c.Done {
			return fmt.Errorf("call %q unknown or already done", m.GetCallId())
		}
		if c.Started() && slices.Contains(c.StartedAttempts, m.GetAttempt()) {
			return fmt.Errorf("call %q started twice in attempt %d", m.GetCallId(), m.GetAttempt())
		}
		if c.Approval != nil && !c.Approval.Approved {
			return fmt.Errorf("call %q started without approval", m.GetCallId())
		}
		if c.NodeID != "" && m.GetNodeId() != c.NodeID {
			return fmt.Errorf("call %q re-routed from node %s", m.GetCallId(), c.NodeID)
		}
		c.StartedAttempts = append(c.StartedAttempts, m.GetAttempt())
		c.StartedReadOnly = append(c.StartedReadOnly, m.GetReadOnly())
		if m.GetNodeId() != "" {
			if c.NodeID == "" {
				c.DispatchedAt = e.GetTime().AsTime()
			}
			c.NodeID, c.Deadline = m.GetNodeId(), m.GetDeadline().AsTime()
		}

	case *v1.Event_ToolResult:
		m := p.ToolResult
		var r *Run
		var err error
		if m.GetAttempt() == 0 {
			// 外部结果：只接受已派发的路由调用。
			r, err = s.activeRun(m.GetRunId())
		} else {
			r, err = s.fencedRun(m.GetRunId(), m.GetAttempt())
		}
		if err != nil {
			return err
		}
		c := r.call(m.GetCallId())
		if c == nil || c.Done {
			return fmt.Errorf("call %q unknown or already done", m.GetCallId())
		}
		if m.GetAttempt() == 0 && !c.Dispatched() {
			return fmt.Errorf("external result for call %q that was not dispatched to a node", m.GetCallId())
		}
		if m.GetInputId() != "" && m.GetAttempt() != 0 {
			return fmt.Errorf("input id on a worker result for call %q", m.GetCallId())
		}
		// 卡片只能由平台校验后写入 show_card 的结果（ADR-0032）：设备、用户不能伪造卡片。
		if hasCard(m.GetContent()) && (m.GetAttempt() == 0 || c.Call.GetCapability() != ShowCardCapability) {
			return fmt.Errorf("card in the result of call %q (%s)", m.GetCallId(), c.Call.GetCapability())
		}
		// 旁听片段带结构化说话人，佩戴者的话会被推导为用户原话（Provenance）；它只能来自平台渲染、审核过的 attested 页
		// （ADR-0052），设备、MCP 的普通结果或内置工具都不能写入。
		if hasCapture(m.GetContent()) && (m.GetAttempt() == 0 || !m.GetAttested()) {
			return fmt.Errorf("capture segment in the result of call %q (%s)", m.GetCallId(), c.Call.GetCapability())
		}
		if m.GetAttested() && (m.GetAttempt() == 0 || !capname.IsMCP(c.Call.GetCapability()) || c.NodeID != "" || m.GetIsError() || m.GetFromCall() != "" || m.GetBackgrounded() != nil) {
			return fmt.Errorf("invalid attested result")
		}
		for _, b := range m.GetContent() {
			if g := b.GetCapture(); g != nil {
				if (m.GetAttested() && !ValidAttestedRef(g.GetRef())) || (!m.GetAttested() && g.GetRef() != "") {
					return fmt.Errorf("invalid capture result ref")
				}
				if err := validateCaptureSegment(g); err != nil {
					return err
				}
			}
		}
		// 卡片动作同样只由平台写入：它只出现在用户等回答时点卡片而得的 ask_user 回答里，这条回答必有输入 ID
		// （由动作推导）。其他结果带上它，客户端会把没有执行过的动作显示为已选。
		if (m.GetCardAction() != nil || hasCardAction(m.GetContent())) && (m.GetAttempt() != 0 || c.NodeID != askuser.NodeID || m.GetInputId() == "") {
			return fmt.Errorf("card action in the result of call %q (%s)", m.GetCallId(), c.Call.GetCapability())
		}
		if err := s.checkCardAction(m.GetCardAction()); err != nil {
			return err
		}
		if err := s.checkCardActions(m.GetContent()); err != nil {
			return err
		}
		if b := m.GetBackgrounded(); b != nil {
			if err := s.checkBackgrounded(m, c); err != nil {
				return err
			}
			c.Background = true
			if s.Background == nil {
				s.Background = map[string]*BackgroundCall{}
			}
			s.Background[m.GetCallId()] = &BackgroundCall{CallID: m.GetCallId(), RunID: r.ID, NodeID: b.GetNodeId(),
				Capability: c.Call.GetCapability(), Deadline: b.GetDeadline().AsTime()}
		}
		if err := s.recordInput(InputRef{ID: m.GetInputId(), RunID: r.ID, Steered: true, Answered: m.GetCallId()}); err != nil {
			return err
		}
		c.Done = true
		if m.GetAttempt() == 0 && c.NodeID == askuser.NodeID {
			r.Resume = true // 用户回答了提问
		}
		r.StalledTakeovers = 0
		s.History = append(s.History, e)

	case *v1.Event_MemoryRecalled:
		m := p.MemoryRecalled
		r, err := s.fencedRun(m.GetRunId(), m.GetAttempt())
		if err != nil {
			return err
		}
		if r.Recalled || r.Turns > 0 {
			return fmt.Errorf("run %s: memory recalled twice or after the first turn", r.ID)
		}
		r.Recall, r.Recalled = m.GetItems(), true
		r.StalledTakeovers = 0

	case *v1.Event_ContextCompacted:
		m := p.ContextCompacted
		r, err := s.fencedRun(m.GetRunId(), m.GetAttempt())
		if err != nil {
			return err
		}
		if c := r.PendingCall(); c != nil {
			return fmt.Errorf("context compacted while call %s is pending", c.Call.GetCallId())
		}
		if err := s.checkCut(m.GetThroughSeq()); err != nil {
			return err
		}
		i := 0
		for i < len(s.History) && s.History[i].GetSeq() <= m.GetThroughSeq() {
			i++
		}
		s.History = slices.Clone(s.History[i:])
		s.Compaction, s.CompactedAt = m, e.GetSeq()
		r.Compactions++
		r.StalledTakeovers = 0

	case *v1.Event_DecisionMade:
		m := p.DecisionMade
		r, err := s.fencedRun(m.GetRunId(), m.GetAttempt())
		if err != nil {
			return err
		}
		k := DecisionKey(m.GetKind(), m.GetSubject())
		if m.GetKind() == "" || m.GetSubject() == "" {
			return fmt.Errorf("decision without kind or subject")
		}
		if _, dup := r.Decisions[k]; dup {
			return fmt.Errorf("decision %s made twice", k)
		}
		if m.GetChosen() != "" && !slices.Contains(m.GetOptions(), m.GetChosen()) {
			return fmt.Errorf("decision %s chose %q outside its options", k, m.GetChosen())
		}
		if r.Decisions == nil {
			r.Decisions = map[string]string{}
		}
		r.Decisions[k] = m.GetChosen()

	case *v1.Event_BackgroundCancelled:
		m := p.BackgroundCancelled
		b := s.Background[m.GetCallId()]
		if b == nil {
			return fmt.Errorf("call %q is not an open background call", m.GetCallId())
		}
		if m.GetCapability() != b.Capability {
			return fmt.Errorf("background call %q is %s, not %s", m.GetCallId(), b.Capability, m.GetCapability())
		}
		s.settleBackground(m.GetCallId())
		s.History = append(s.History, e)

	case *v1.Event_RunCompleted:
		r, err := s.fencedRun(p.RunCompleted.GetRunId(), p.RunCompleted.GetAttempt())
		if err != nil {
			return err
		}
		if c := r.PendingCall(); c != nil {
			return fmt.Errorf("run completed while call %s is pending", c.Call.GetCallId())
		}
		r.Status = RunCompleted

	case *v1.Event_RunFailed:
		r, err := s.fencedRun(p.RunFailed.GetRunId(), p.RunFailed.GetAttempt())
		if err != nil {
			return err
		}
		r.Status = RunFailed

	case *v1.Event_RunInterrupted:
		r, err := s.activeRun(p.RunInterrupted.GetRunId())
		if err != nil {
			return err
		}
		r.Status = RunInterrupted

	default:
		return fmt.Errorf("unknown payload %T", p)
	}
	s.Seq = e.GetSeq()
	return nil
}

func checkArtifactCopied(sessionID string, m *v1.ArtifactCopied) error {
	if m.GetCallId() == "" || m.GetSourceArtifactId() == "" || m.GetArtifactId() == "" || m.GetSourceSessionId() == "" {
		return fmt.Errorf("ArtifactCopied requires call, source and artifact")
	}
	if m.GetSourceSessionId() == sessionID {
		return fmt.Errorf("ArtifactCopied from the same session")
	}
	slot, err := shard.SlotOf(m.GetArtifactId())
	if err != nil {
		return fmt.Errorf("ArtifactCopied artifact: %w", err)
	}
	if own, err := shard.SessionSlotOf(sessionID); err != nil || own != slot {
		return fmt.Errorf("ArtifactCopied artifact %s is not in this session's slot", m.GetArtifactId())
	}
	return nil
}

// CanCut 报告 History 能否在 seq 处截断：seq 之后的调用结果都不指向 seq 及之前请求的调用，
// 这样截断后的上下文中不会出现没有请求的孤立结果。调用方还须保证此时没有未完成的调用。
func (s *State) CanCut(seq uint64) bool { return s.checkCut(seq) == nil }

func (s *State) checkCut(seq uint64) error {
	prev := uint64(0)
	if s.Compaction != nil {
		prev = s.Compaction.GetThroughSeq()
	}
	if seq <= prev || seq > s.Seq {
		return fmt.Errorf("compaction through seq %d out of range (%d, %d]", seq, prev, s.Seq)
	}
	before := map[string]bool{}
	for _, e := range s.History {
		switch p := e.GetPayload().(type) {
		case *v1.Event_AssistantMessage:
			if e.GetSeq() <= seq {
				for _, tc := range p.AssistantMessage.GetToolCalls() {
					before[tc.GetCallId()] = true
				}
			}
		case *v1.Event_ToolResult:
			if e.GetSeq() > seq && before[p.ToolResult.GetCallId()] {
				return fmt.Errorf("compaction through seq %d splits call %s from its result", seq, p.ToolResult.GetCallId())
			}
		}
	}
	return nil
}
