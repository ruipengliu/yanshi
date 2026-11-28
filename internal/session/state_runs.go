package session

import (
	"fmt"
	"maps"
	"slices"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/workqueue"
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

type Run struct {
	ID     string
	Status RunStatus
	// RequestedAt 是 RunRequested 的事件时间。
	RequestedAt time.Time
	// Request 与 RequestedSeq 保留原输入及其位置，压缩后也能判断重答资格。
	Request      *v1.RunRequested
	RequestedSeq uint64
	// Steered 记下发生过插话，不能因历史压缩而丢失这个事实。
	Steered bool
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
	// Compactions 是该 Run 中的上下文压缩次数；OutputTokens 是已提交的 AssistantMessage 累计的输出 token。
	// 二者与 Turns 一起衡量 Run 已经做了多少工作（工作类别，ADR-0029）。
	Compactions  int
	OutputTokens int64
	// Resume 表示用户刚做出审批或回答，下一个 Attempt 尚未开始：长任务因此在后台池中先被认领（恢复名额，
	// 不可叠加，由下一个 AttemptStarted 消费）。
	Resume bool
	// Decisions 是本 Run 中已作出的判断（DecisionMade，ADR-0030），键为 DecisionKey(种类, 对象)，值为选中的选项
	// （空表示判断不可用、走了默认路径）。同一对象的同一种判断只作一次，重放与接管都读它而不重新判断。
	Decisions map[string]string
	// Recall 是 Run 开始时召回的 Memory；Recalled 表示已经召回过（结果可能为空）。
	Recall   []*v1.RecalledMemory
	Recalled bool
	Calls    []*Call
	// SuspendReason 是最近一次挂起的原因（RunSuspended.reason），开始新 Attempt 时清空：配额用尽时
	// SuspendedUntil 为配额重置时间（docs/design/m4-quota-usage.md §3）；SuspendHandoff 表示移交给其他 Worker。
	SuspendReason  string
	SuspendedUntil time.Time
	// ModerationWaits 是本 Run 自上一条 AssistantMessage 以来，因输出审核不可用而连续挂起的次数；
	// ModerationWaitSince 是其中第一次挂起的时间（事件时间）。二者决定退避与等待上限（ADR-0054），
	// 跨 Attempt 保留，写入 AssistantMessage 时清零。
	ModerationWaits     int
	ModerationWaitSince time.Time
	// FromBackground 非空表示 Run 由该后台调用的了结（结果或到期）开启，而不是用户的输入（ADR-0035）。
	FromBackground string
	// LoopState 与 LoopStateVersion 是本 Run 最近一条 AssistantMessage 带的外部循环状态（ADR-0062 第 2 条）：
	// 下一步原样带回。存在投影中而不是从 History 读取，压缩不会影响它。
	LoopState        []byte
	LoopStateVersion string
}

// MaxLoopState 是外部循环状态的上限（解码后的字节数，ADR-0062 第 2 条）。Worker 在写日志之前校验回答，这里再查一次。
const MaxLoopState = 64 << 10

// SuspendHandoff 是移交的挂起原因（ADR-0029）：Worker 正常结束 Attempt 并立即归还 Session，由其他 Worker 恢复，
// 如交互 Worker 移交成为长任务的 Run、进程优雅停机。
const SuspendHandoff = "handoff"

// SuspendModeration 是输出审核不可用时的挂起原因（ADR-0054）：回复未写入日志，SuspendedUntil 到期后
// 重新生成并送检。
const SuspendModeration = "moderation_unavailable"

// QuotaSuspended 报告 Run 是否因配额用尽而挂起。
func (r *Run) QuotaSuspended() bool {
	return r.Status == RunSuspended && r.SuspendReason != "" && r.SuspendReason != SuspendHandoff &&
		r.SuspendReason != SuspendModeration
}

// ModerationSuspended 报告 Run 是否因输出审核不可用而挂起（ADR-0054）。
func (r *Run) ModerationSuspended() bool {
	return r.Status == RunSuspended && r.SuspendReason == SuspendModeration
}

// DecisionKey 是 Run.Decisions 的键。
func DecisionKey(kind, subject string) string { return kind + "/" + subject }

func (r *Run) clone() *Run {
	cp := *r
	cp.Decisions = maps.Clone(r.Decisions)
	cp.Calls = make([]*Call, len(r.Calls))
	for i, c := range r.Calls {
		cc := *c
		cc.StartedAttempts = slices.Clone(c.StartedAttempts)
		cc.StartedReadOnly = slices.Clone(c.StartedReadOnly)
		if c.Approval != nil {
			a := *c.Approval
			cc.Approval = &a
		}
		cp.Calls[i] = &cc
	}
	return &cp
}

// KeepRuns 是新 Run 请求时投影中保留完整明细的旧 Run 数。活跃推进只用到当前 Run；更早的 Run 只剩摘要，
// 调用明细从日志读取（State.Past、Store.HistoryCall）。
const KeepRuns = 2

// RunSummary 是移出投影的终态 Run 的摘要：历史接口（Session 的 Run 列表）与按 ID 的查询仍然可用。
type RunSummary struct {
	ID          string
	Status      RunStatus
	RequestedAt time.Time
	Attempt     uint32
	Turns       int
	// FromBackground 同 Run.FromBackground。
	FromBackground string
}

func (r *Run) summary() RunSummary {
	return RunSummary{ID: r.ID, Status: r.Status, RequestedAt: r.RequestedAt, Attempt: r.Attempt, Turns: r.Turns,
		FromBackground: r.FromBackground}
}

// Summaries 返回全部 Run 的摘要，按请求顺序。
func (s *State) Summaries() []RunSummary {
	out := slices.Clone(s.Past)
	for _, r := range s.Runs {
		out = append(out, r.summary())
	}
	return out
}

// RunSummary 返回 ID 为 id 的 Run 的摘要（含已移出投影的），没有时 ok 为 false。
func (s *State) RunSummary(id string) (RunSummary, bool) {
	if r := s.Run(id); r != nil {
		return r.summary(), true
	}
	for _, p := range s.Past {
		if p.ID == id {
			return p, true
		}
	}
	return RunSummary{}, false
}

// trimRuns 在新 Run 请求时调用：只留最近 KeepRuns 个旧 Run（它们都已终态），更早的移出投影，只留摘要。
func (s *State) trimRuns() {
	if n := len(s.Runs) - 1 - KeepRuns; n > 0 {
		for _, r := range s.Runs[:n] {
			s.Past = append(s.Past, r.summary())
		}
		s.Runs = slices.Clone(s.Runs[n:])
	}
}

// Active 返回唯一的非终态 Run，没有则返回 nil。
func (s *State) Active() *Run {
	if n := len(s.Runs); n > 0 && !s.Runs[n-1].Status.Terminal() {
		return s.Runs[n-1]
	}
	return nil
}

// WorkClass 是推进本 Session 的工作在队列中的类别（ADR-0029）：按活跃 Run 已经做的工作（模型回合与压缩次数、
// 输出 token）判定是否为长任务，见 workqueue.Classify。
func (s *State) WorkClass() workqueue.Class {
	var w workqueue.Work
	if a := s.Active(); a != nil {
		w = workqueue.Work{Turns: a.Turns + a.Compactions, OutputTokens: a.OutputTokens, Resume: a.Resume}
	}
	return workqueue.Classify(s.Created.GetBusinessLine(), w)
}

func (s *State) Run(id string) *Run {
	for _, r := range s.Runs {
		if r.ID == id {
			return r
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
