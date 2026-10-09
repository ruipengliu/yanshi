// Package session 把 Session 的 Event 日志投影为状态。
//
// 投影是纯函数，并在投影过程中校验 docs/design/m0-core-primitives.md 中定义的不变量：
// 任何违反状态机的事件都会使 Apply 返回错误。写入方在追加前用它校验，
// 模拟测试用它检查日志始终合法。
package session

import (
	"fmt"
	"slices"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

type RunStatus int

const (
	RunQueued RunStatus = iota
	RunRunning
	// RunSuspended 表示 Run 在等待外部条件（审批、设备结果），没有 Worker 持有它。
	RunSuspended
	RunCompleted
	RunFailed
	RunInterrupted
)

func (s RunStatus) String() string {
	switch s {
	case RunQueued:
		return "queued"
	case RunRunning:
		return "running"
	case RunSuspended:
		return "suspended"
	case RunCompleted:
		return "completed"
	case RunFailed:
		return "failed"
	case RunInterrupted:
		return "interrupted"
	}
	return fmt.Sprintf("RunStatus(%d)", int(s))
}

func (s RunStatus) Terminal() bool { return s >= RunCompleted }

// Call 是 Run 中一次 Capability 调用的进度。
type Call struct {
	Call *v1.ToolCall
	// StartedAttempts 记录哪些 Attempt 追加过 ToolCallStarted；长度 > 1 表示被重新执行过。
	StartedAttempts []uint32
	// NodeID 非空表示路由调用，Deadline 是其结果截止时间。
	NodeID   string
	Deadline time.Time
	Done     bool

	Approval *Approval
}

// Approval 是一次调用的审批进度；nil 表示未请求审批。
type Approval struct {
	Summary  string
	Deadline time.Time
	Decided  bool
	Approved bool
}

// AwaitingApproval 表示已请求审批、尚无决定且调用未完成（未因超时等原因结束）。
func (c *Call) AwaitingApproval() bool { return c.Approval != nil && !c.Approval.Decided && !c.Done }

// Dispatched 表示路由调用已派发、尚无结果。
func (c *Call) Dispatched() bool { return c.NodeID != "" && c.Started() && !c.Done }

func (c *Call) Started() bool { return len(c.StartedAttempts) > 0 }

type Run struct {
	ID     string
	Status RunStatus
	// RequestedAt 是 RunRequested 的事件时间。
	RequestedAt time.Time
	// Attempt 是当前 Attempt 号；0 表示尚未有 Worker 认领。
	Attempt uint32
	// LiveEndpoint 是当前 Attempt 的执行进程地址，实时增量从这里拉取（ADR-0013）。
	LiveEndpoint string
	// Takeovers 是在 running 状态下开启新 Attempt 的次数，即上一个 Worker 未正常结束的次数。
	Takeovers int
	// StalledTakeovers 是自上次进展（上下文前进：模型输出、调用结果、上下文压缩）以来的接管次数，
	// 用于识别反复弄崩 Worker 的 Run；小时级 Run 期间零散的接管（如 Worker 滚动发布）不会累积。
	// ToolCallStarted 不算进展，否则"执行即崩溃"的幂等调用会无限重试。
	StalledTakeovers int
	// Turns 是该 Run 已提交的 AssistantMessage 数。
	Turns int
	// Recall 是 Run 开始时召回的 Memory；Recalled 表示已经召回过（结果可能为空）。
	Recall   []*v1.RecalledMemory
	Recalled bool
	Calls    []*Call
	// SuspendReason 是最近一次挂起的原因（RunSuspended.reason），开始新 Attempt 时清空；
	// 非空表示因配额挂起，SuspendedUntil 为配额重置时间（docs/design/m4-quota-usage.md §3）。
	SuspendReason  string
	SuspendedUntil time.Time
}

// PendingCall 返回第一个尚无结果的调用，没有则返回 nil。
func (r *Run) PendingCall() *Call {
	for _, c := range r.Calls {
		if !c.Done {
			return c
		}
	}
	return nil
}

func (r *Run) call(id string) *Call {
	for _, c := range r.Calls {
		if c.Call.GetCallId() == id {
			return c
		}
	}
	return nil
}

func (r *Run) clone() *Run {
	cp := *r
	cp.Calls = make([]*Call, len(r.Calls))
	for i, c := range r.Calls {
		cc := *c
		cc.StartedAttempts = slices.Clone(c.StartedAttempts)
		if c.Approval != nil {
			a := *c.Approval
			cc.Approval = &a
		}
		cp.Calls[i] = &cc
	}
	return &cp
}

// State 是 Session 在某个 seq 处的投影。
type State struct {
	SessionID string
	// Seq 是已应用的最后一个事件的 seq；0 表示空日志。
	Seq     uint64
	Created *v1.SessionCreated
	Runs    []*Run
	// History 是构成对话上下文的事件（用户输入、模型输出、调用结果），按日志顺序；
	// 只含最近一次 Compaction 之后的事件，因此随上下文窗口有界，而不随日志增长。
	History []*v1.Event
	// Closed 非 nil 表示 Session 已关闭（终态）。
	Closed *v1.SessionClosed
	// Compaction 是最近一次上下文压缩，nil 表示从未压缩（ADR-0012）；CompactedAt 是该事件自身的 seq。
	Compaction  *v1.ContextCompacted
	CompactedAt uint64
	// callIDs 是 Session 内出现过的全部调用 ID；调用 ID 在 Session 内必须唯一。
	callIDs map[string]bool
}

// Reduce 从空状态依次应用 events。
func Reduce(events []*v1.Event) (*State, error) {
	s := &State{}
	for _, e := range events {
		if err := s.Apply(e); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Active 返回唯一的非终态 Run，没有则返回 nil。
func (s *State) Active() *Run {
	if n := len(s.Runs); n > 0 && !s.Runs[n-1].Status.Terminal() {
		return s.Runs[n-1]
	}
	return nil
}

func (s *State) Run(id string) *Run {
	for _, r := range s.Runs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// Clone 返回可独立修改的副本；事件本身不可变，因此共享。
func (s *State) Clone() *State {
	cp := *s
	cp.Runs = make([]*Run, len(s.Runs))
	for i, r := range s.Runs {
		cp.Runs[i] = r.clone()
	}
	cp.History = slices.Clip(s.History)
	cp.callIDs = make(map[string]bool, len(s.callIDs))
	for id := range s.callIDs {
		cp.callIDs[id] = true
	}
	return &cp
}

// Check 校验 events 能否依次应用到 s 之后，不修改 s。events 的 seq 可以留空，按顺序补齐后校验。
func (s *State) Check(events ...*v1.Event) error {
	cp := s.Clone()
	for i, e := range events {
		if e.GetSeq() == 0 {
			e = withSeq(e, s.Seq+uint64(i)+1)
		}
		if err := cp.Apply(e); err != nil {
			return err
		}
	}
	return nil
}

func withSeq(e *v1.Event, seq uint64) *v1.Event {
	cp := &v1.Event{Id: e.GetId(), SessionId: e.GetSessionId(), Seq: seq, Time: e.GetTime()}
	cp.Payload = e.Payload
	return cp
}

// Apply 把一个事件应用到状态上；事件违反状态机时返回错误且状态不变。
func (s *State) Apply(e *v1.Event) error {
	if err := s.apply(e); err != nil {
		return fmt.Errorf("session %s seq %d: %w", e.GetSessionId(), e.GetSeq(), err)
	}
	return nil
}

func (s *State) apply(e *v1.Event) error {
	if e.GetSeq() != s.Seq+1 {
		return fmt.Errorf("seq gap: want %d", s.Seq+1)
	}
	if s.Created == nil {
		c := e.GetSessionCreated()
		if c == nil {
			return fmt.Errorf("first event must be SessionCreated, got %T", e.GetPayload())
		}
		s.SessionID, s.Created, s.Seq = e.GetSessionId(), c, e.GetSeq()
		return nil
	}
	if e.GetSessionId() != s.SessionID {
		return fmt.Errorf("event belongs to session %q", e.GetSessionId())
	}
	if s.Closed != nil {
		return fmt.Errorf("session is closed")
	}

	switch p := e.GetPayload().(type) {
	case *v1.Event_SessionCreated:
		return fmt.Errorf("duplicate SessionCreated")

	case *v1.Event_SessionClosed:
		// 关闭方须先中断活跃的 Run（可与本事件同批提交）。
		if a := s.Active(); a != nil {
			return fmt.Errorf("session closed while run %s is active", a.ID)
		}
		s.Closed = p.SessionClosed

	case *v1.Event_RunRequested:
		if a := s.Active(); a != nil {
			return fmt.Errorf("run %s requested while run %s is active", p.RunRequested.GetRunId(), a.ID)
		}
		if p.RunRequested.GetRunId() == "" || s.Run(p.RunRequested.GetRunId()) != nil {
			return fmt.Errorf("invalid or duplicate run id %q", p.RunRequested.GetRunId())
		}
		s.Runs = append(s.Runs, &Run{ID: p.RunRequested.GetRunId(), Status: RunQueued, RequestedAt: e.GetTime().AsTime()})
		s.History = append(s.History, e)

	case *v1.Event_Steered:
		if _, err := s.activeRun(p.Steered.GetRunId()); err != nil {
			return err
		}
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
		r.SuspendReason, r.SuspendedUntil = "", time.Time{}

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

	case *v1.Event_AssistantMessage:
		m := p.AssistantMessage
		r, err := s.fencedRun(m.GetRunId(), m.GetAttempt())
		if err != nil {
			return err
		}
		if c := r.PendingCall(); c != nil {
			return fmt.Errorf("assistant message while call %s is pending", c.Call.GetCallId())
		}
		seen := map[string]bool{}
		for _, tc := range m.GetToolCalls() {
			if tc.GetCallId() == "" || seen[tc.GetCallId()] || s.callIDs[tc.GetCallId()] {
				return fmt.Errorf("invalid or duplicate call id %q", tc.GetCallId())
			}
			seen[tc.GetCallId()] = true
		}
		if s.callIDs == nil {
			s.callIDs = map[string]bool{}
		}
		for _, tc := range m.GetToolCalls() {
			r.Calls = append(r.Calls, &Call{Call: tc})
			s.callIDs[tc.GetCallId()] = true
		}
		r.Turns++
		r.StalledTakeovers = 0
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
		if m.GetNodeId() != "" {
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
		c.Done = true
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
		r.StalledTakeovers = 0

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

func (s *State) activeRun(id string) (*Run, error) {
	r := s.Run(id)
	if r == nil {
		return nil, fmt.Errorf("unknown run %q", id)
	}
	if r.Status.Terminal() {
		return nil, fmt.Errorf("run %s is %s", id, r.Status)
	}
	return r, nil
}

// fencedRun 要求 run 处于 running 且 attempt 是当前 Attempt：旧 Attempt 的事件一律拒绝。
func (s *State) fencedRun(id string, attempt uint32) (*Run, error) {
	r, err := s.activeRun(id)
	if err != nil {
		return nil, err
	}
	if r.Status != RunRunning || attempt != r.Attempt {
		return nil, fmt.Errorf("stale attempt %d for run %s (current %d, %s)", attempt, id, r.Attempt, r.Status)
	}
	return r, nil
}
