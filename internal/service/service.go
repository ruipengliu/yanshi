// Package service 是 Session 的对外操作：创建、输入、中断、查询。
// HTTP API 与模拟测试客户端都经由这里写日志，二者走同一条代码路径。
package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/askuser"
	"yanshi/internal/eventlog"
	"yanshi/internal/faults"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid argument")
)

// Canceler 撤回已派发到 Node 的调用（由 node.Hub 实现）。
type Canceler interface {
	Cancel(ctx context.Context, nodeID, callID string) error
}

type Service struct {
	// Store 须配置 Waking：输入、审批与回答在写日志的同一事务中唤醒 Session（CommitAndWake）。
	Store  *session.Store
	Agents *agentdef.Registry
	// Nodes 为 nil 时中断不撤回设备调用。
	Nodes Canceler

	// 生命周期（docs/design/m2-session-lifecycle.md）：Index 与 Deletions 为 nil 时不维护索引、
	// 不支持删除；Janitor 是清理队列，关闭与删除后入队由 Janitor 回收资源。
	Index     lifecycle.Index
	Deletions lifecycle.Deletions
	Janitor   workqueue.Queue
	// Quotas 非空时，配额用尽的业务线或 EndUser 不能开始新 Run（docs/design/m4-quota-usage.md §3）。
	Quotas *usage.Quotas
	// Admission 非空时，交互工作积压过高的业务线暂不接受新 Run（ErrOverloaded）。
	Admission Admission
	// RecentDevice 非空时，没有注明来源设备的输入注明 EndUser 最近在前台使用的设备（设备歧义消解，
	// docs/design/m6-decisions.md §3）；返回空串表示不知道。经 Connection 提交的输入由网关注明来源设备。
	RecentDevice func(ctx context.Context, businessLine, endUser string) string
	// Moderator 非空时，输入（新 Run 与插话）在写日志之前检查，违规的不写入（ADR-0021）。
	Moderator moderation.Moderator
	// Artifacts 非空时，输入中的 Media 须引用本 Session 的工件，其类型、大小与名称取自工件的元数据。
	Artifacts ArtifactStat
}

// ArtifactStat 查询工件元数据（artifact.Service）。
type ArtifactStat interface {
	Stat(ctx context.Context, id string) (*artifact.Meta, error)
}

// ErrConflict 表示请求与 Session 当前状态冲突（如审批已决定）。
var ErrConflict = errors.New("conflict")

type CreateRequest struct {
	BusinessLine string
	EndUser      string
	// Agent 是 AgentDef 名称；AgentVersion 为空时取默认版本。
	Agent        string
	AgentVersion string
	// SessionID 与 Parent 只用于委托派生的子 Session（docs/design/m5-multi-agent.md）：ID 由父调用推导，
	// 重复创建返回已有的 Session（幂等）。
	SessionID string
	Parent    *v1.ParentRef
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (string, error) {
	if req.BusinessLine == "" || req.EndUser == "" {
		return "", fmt.Errorf("%w: business_line and end_user are required", ErrInvalid)
	}
	var def *agentdef.Def
	var err error
	// 未指定版本时按发布配置分流（docs/design/m4-agent-rollout.md §3）；撤回的版本不能指定。
	if req.AgentVersion == "" {
		def, err = s.Agents.Resolve(req.Agent, req.EndUser)
	} else {
		ref := &v1.AgentRef{Name: req.Agent, Version: req.AgentVersion}
		if def, err = s.Agents.Get(ref); err == nil {
			if _, withdrawn := s.Agents.Withdrawn(ref); withdrawn {
				err = fmt.Errorf("agent %s@%s is withdrawn", req.Agent, req.AgentVersion)
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	st := &session.State{SessionID: req.SessionID}
	if st.SessionID == "" {
		st.SessionID = "ses_" + s.Store.IDs()
	}
	// 先写索引、再写日志：日志写入失败只会留下一条找不到日志的索引（列表与删除都能处理），
	// 反过来则会出现索引之外的 Session，按 EndUser 删除时会被遗漏。
	if s.Index != nil {
		now := s.Store.Clock.Now()
		if err := s.Index.Put(ctx, &lifecycle.Session{ID: st.SessionID, BusinessLine: req.BusinessLine, EndUser: req.EndUser,
			Agent: def.Name, CreatedAt: now, LastInputAt: now}); err != nil {
			return "", err
		}
	}
	err = s.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{
		BusinessLine: req.BusinessLine, EndUser: req.EndUser, Agent: def.Ref(), Parent: req.Parent,
	}}})
	if errors.Is(err, eventlog.ErrConflict) && req.SessionID != "" {
		// 指定 ID 的 Session 已存在：是同一次委托的重复创建时返回它。
		if existing, lerr := s.Store.Load(ctx, req.SessionID); lerr == nil && existing.Created != nil &&
			existing.Created.GetParent().GetCallId() == req.Parent.GetCallId() && existing.Created.GetParent().GetSessionId() == req.Parent.GetSessionId() {
			return req.SessionID, nil
		}
		return "", fmt.Errorf("%w: session %s already exists", ErrConflict, req.SessionID)
	}
	if err != nil {
		return "", err
	}
	return st.SessionID, nil
}

type accessKey struct{}

// WithAccess 使 ctx 中的 Service 调用只作用于 allows 允许的 Session（按业务线与 EndUser），其余一律视为不存在
// （ErrNotFound）。入口（HTTP API、Connection）以此校验归属，而不是先读一遍 Session 再调用：每次读取是快照、
// 日志与删除记录三次查询（延展性评审 §4.3）。
func WithAccess(ctx context.Context, allows func(businessLine, endUser string) bool) context.Context {
	return context.WithValue(ctx, accessKey{}, allows)
}

// Load 返回 Session 的当前投影；ctx 带有 WithAccess 时同时校验归属。
func (s *Service) Load(ctx context.Context, sessionID string) (*session.State, error) {
	st, err := s.Store.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if st.Created == nil {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	if allows, _ := ctx.Value(accessKey{}).(func(string, string) bool); allows != nil && !allows(st.Created.GetBusinessLine(), st.Created.GetEndUser()) {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	// 有删除记录的 Session 对外不可见，即使清理尚未完成（ADR-0015）。
	if s.Deletions != nil {
		if gone, err := lifecycle.Deleted(ctx, s.Deletions, sessionID); err != nil || gone {
			if gone {
				return nil, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
			}
			return nil, err
		}
	}
	return st, nil
}

type SubmitResult struct {
	// Duplicate 为 true 表示同一 InputID 的输入此前已生效：本次没有写入，其余字段是首次的结果。
	Duplicate bool
	RunID     string
	// Steered 为 true 表示输入并入了进行中的 Run，而非开启新 Run。
	Steered bool
	// Answered 非空表示输入作为对该 ask_user 提问的回答（ADR-0025）：Run 正在等用户回答时，
	// 用户直接打字就是在回答，而不是插话。
	Answered string
}

// MaxInputIDLen 是客户端输入 ID 的长度上限。
const MaxInputIDLen = 128

// Submit 提交用户输入：有活跃 Run 时作为 Steer，否则开启新 Run。
func (s *Service) Submit(ctx context.Context, sessionID string, input []*v1.ContentBlock) (*SubmitResult, error) {
	return s.SubmitWithID(ctx, sessionID, "", input)
}

// SubmitWithID 与 Submit 相同，但带客户端生成的输入 ID：结果未知（连接断开）时以同一 ID 重新提交是安全的，
// 已生效的输入返回首次的结果（Duplicate），不会重复写入。去重窗口是最近 session.MaxRecentInputs 条输入。
func (s *Service) SubmitWithID(ctx context.Context, sessionID, inputID string, input []*v1.ContentBlock) (*SubmitResult, error) {
	return s.submit(ctx, sessionID, inputID, "", input, false)
}

// SubmitDelegated 提交委托给子 Session 的任务（docs/design/m5-multi-agent.md）：语义同 SubmitWithID，但不经 Submit
// 准入——子 Run 是进行中的父 Run 的一部分，不是新的交互请求。配额照常检查。
func (s *Service) SubmitDelegated(ctx context.Context, sessionID, inputID string, input []*v1.ContentBlock) (*SubmitResult, error) {
	return s.submit(ctx, sessionID, inputID, "", input, true)
}

// SubmitFromCall 提交 Call 中语音模型派生的任务（run_task，docs/design/m3-call.md §5）：语义同 SubmitWithID，
// 输入带上 from_call，Run 据此把结果写成适合朗读的形式。Call 已不在进行中时返回 ErrConflict。
func (s *Service) SubmitFromCall(ctx context.Context, sessionID, callID, inputID string, input []*v1.ContentBlock) (*SubmitResult, error) {
	if callID == "" {
		return nil, fmt.Errorf("%w: call id is required", ErrInvalid)
	}
	return s.submit(ctx, sessionID, inputID, callID, input, false)
}

func (s *Service) submit(ctx context.Context, sessionID, inputID, fromCall string, input []*v1.ContentBlock, delegated bool) (*SubmitResult, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrInvalid)
	}
	if len(inputID) > MaxInputIDLen {
		return nil, fmt.Errorf("%w: input_id exceeds %d bytes", ErrInvalid, MaxInputIDLen)
	}
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// 卡片动作的输入 ID 由平台推导：同一版卡片上的同一动作只生效一次，与客户端是否带 ID 无关。
	if id, in, err := s.cardAction(ctx, st, input); err != nil {
		return nil, err
	} else if id != "" {
		inputID, input = id, in
	}
	// 重复提交在任何检查之前返回：首次已经通过了内容安全与配额，结果也已确定。
	if res := duplicate(st, inputID); res != nil {
		return res, nil
	}
	if err := checkUIContext(input); err != nil {
		return nil, err
	}
	if input, err = s.normalizeMedia(ctx, sessionID, input); err != nil {
		return nil, err
	}
	if s.RecentDevice != nil && fromCall == "" {
		if d := s.RecentDevice(ctx, st.Created.GetBusinessLine(), st.Created.GetEndUser()); d != "" {
			input = WithDevice(input, d, false)
		}
	}
	if err := s.moderateInput(ctx, st, input); err != nil {
		return nil, err
	}
	// 准入与配额只拦新 Run：插话属于进行中的 Run，后者在下一次模型调用前自行检查配额。
	if st.Active() == nil && s.Admission != nil && !delegated {
		if err := s.Admission.Admit(ctx, st.Created.GetBusinessLine()); err != nil {
			return nil, err
		}
	}
	if bl := st.Created.GetBusinessLine(); st.Active() == nil && s.Quotas.Enabled(bl) {
		p, err := s.Quotas.Check(ctx, bl, st.Created.GetEndUser(), s.Store.Clock.Now())
		if err != nil {
			return nil, err
		}
		if p != nil {
			metrics.QuotaRejections.WithLabelValues(p.Scope, "submit").Inc()
			return nil, &usage.ExceededError{Period: p}
		}
	}
	var res *SubmitResult
	var prev time.Time
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		// 并发的同 ID 提交可能刚刚生效（冲突后重读到了它）。
		if res = duplicate(st, inputID); res != nil {
			return session.Decision{}, nil
		}
		if st.Closed != nil {
			return session.Decision{}, fmt.Errorf("%w: session %s is closed", ErrConflict, sessionID)
		}
		if fromCall != "" && (st.ActiveCall == nil || st.ActiveCall.ID != fromCall) {
			return session.Decision{}, fmt.Errorf("%w: call %s is not active", ErrConflict, fromCall)
		}
		var events []*v1.Event
		res = &SubmitResult{}
		if a := st.Active(); a != nil {
			res.RunID, res.Steered = a.ID, true
			if c := pendingQuestion(a, ""); c != nil {
				ev, err := typedAnswer(a, c, input)
				if err != nil {
					return session.Decision{}, err
				}
				ev.GetToolResult().InputId, ev.GetToolResult().FromCall = inputID, fromCall
				res.Answered = c.Call.GetCallId()
				events = append(events, ev)
			} else {
				events = append(events, &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: a.ID, Input: input, InputId: inputID, FromCall: fromCall}}})
			}
		} else {
			// 当前版本已撤回：新 Run 从稳定版本开始，切换与 RunRequested 同批提交（ADR-0020）。
			if to, ok := s.Agents.Withdrawn(st.Agent); ok {
				events = append(events, &v1.Event{Payload: &v1.Event_AgentSwitched{AgentSwitched: &v1.AgentSwitched{
					From: st.Agent, To: to.Ref(), Reason: "withdrawn"}}})
			}
			res.RunID = "run_" + s.Store.IDs()
			events = append(events, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: res.RunID, Input: input, InputId: inputID, FromCall: fromCall}}})
		}
		prev = lastRunAt(st)
		// 写日志与入队在同一事务中（CommitAndWake）：二者之间没有可以崩溃的窗口，Run 不会停在 queued。
		return session.Decision{Events: events, Wake: true}, nil
	})
	if err != nil {
		return nil, gone(sessionID, err)
	}
	if res.Duplicate {
		return res, nil
	}
	// 按 TouchEvery 分段：输入与上一个 Run 落在不同段时更新。每段的第一个输入都会写入，索引中的时间因此与
	// 最近的输入在同一段内，滞后不到 TouchEvery；只比较间隔的话，间隔始终短于 TouchEvery 的连续对话永远不会更新。
	if now := s.Store.Clock.Now(); s.Index != nil && (prev.IsZero() || !now.Truncate(lifecycle.TouchEvery).Equal(prev.Truncate(lifecycle.TouchEvery))) {
		if err := s.Index.Touch(ctx, sessionID, now); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Interrupt 中断 Run（用户发起）；Run 已终态时不再写入并返回 nil。
func (s *Service) Interrupt(ctx context.Context, sessionID, runID string) error {
	return s.InterruptBy(ctx, sessionID, runID, "user")
}

// InterruptBy 中断 Run，by 是中断方（RunInterrupted.by，如 "user"、委托级联的 "parent"）。中断之后撤回 Run 中已派发、
// 未完成的路由调用；撤回失败时返回错误，重试同一请求会再撤回一次（Run 已终态，不再写入）：否则被中断的父 Run 的
// 子 Run 会一直跑下去（评审 §11 问题 10）。
func (s *Service) InterruptBy(ctx context.Context, sessionID, runID, by string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	// cancel 是要撤回已派发调用的 Run：本次中断的，或此前已被中断的（重试的请求再撤回一次）；
	// interrupted 表示本次写入了中断。
	var cancel *session.Run
	interrupted := false
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		cancel, interrupted = nil, false
		r := st.Run(runID)
		if r == nil {
			if _, ok := st.RunSummary(runID); ok {
				return session.Decision{}, nil // 已移出投影的 Run 都是终态
			}
			return session.Decision{}, fmt.Errorf("%w: run %s", ErrNotFound, runID)
		}
		if r.Status.Terminal() {
			if r.Status == session.RunInterrupted {
				cancel = r
			}
			return session.Decision{}, nil
		}
		cancel, interrupted = r, true
		return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_RunInterrupted{
			RunInterrupted: &v1.RunInterrupted{RunId: runID, By: by},
		}}}, Wake: wakeOnEnding(st)}, nil
	})
	if err != nil {
		return gone(sessionID, err)
	}
	if interrupted {
		metrics.RunsFinished.WithLabelValues("interrupted", agentdef.Label(st.Agent)).Inc()
	}
	if cancel != nil {
		return s.cancelDispatched(ctx, cancel)
	}
	return nil
}

// wakeOnEnding 报告结束 Run 的写入是否同时唤醒 Session：委托派生的子 Session 需要它的 Worker 把终态写回父调用
// （docs/design/m5-multi-agent.md §4）。
func wakeOnEnding(st *session.State) bool {
	return st.Created.GetParent() != nil
}

// afterClose 是关闭之后的步骤：撤回被关闭中断的 Run 中已派发的调用、在索引中记下关闭、交给 Janitor 销毁沙箱工作区。
// 每一步都幂等；任一步失败时返回错误，重试 Close 会把它们再做一遍。以前重试遇到"已关闭"直接返回，失败的那一步
// 及其后的步骤就再也不会执行。
func (s *Service) afterClose(ctx context.Context, st *session.State) error {
	if n := len(st.Runs); n > 0 && st.Runs[n-1].Status == session.RunInterrupted {
		if err := s.cancelDispatched(ctx, st.Runs[n-1]); err != nil {
			return err
		}
	}
	if s.Index != nil {
		if err := s.Index.MarkClosed(ctx, st.SessionID, s.Store.Clock.Now()); err != nil {
			return err
		}
	}
	return s.enqueueJanitor(ctx, st.SessionID, st.Created.GetBusinessLine())
}

// cancelDispatched 尽力撤回 Run 中已派发、未完成的路由调用。提问不经 Inbox，无需撤回（ADR-0025）。
func (s *Service) cancelDispatched(ctx context.Context, r *session.Run) error {
	if s.Nodes == nil {
		return nil
	}
	for _, c := range r.Calls {
		if c.Dispatched() && c.NodeID != askuser.NodeID {
			if err := s.Nodes.Cancel(ctx, c.NodeID, c.Call.GetCallId()); err != nil {
				return err
			}
		}
	}
	return nil
}

// lastRunAt 是最近一个 Run 的请求时间，没有时为零值。
func lastRunAt(st *session.State) time.Time {
	if n := len(st.Runs); n > 0 {
		return st.Runs[n-1].RequestedAt
	}
	return time.Time{}
}

// duplicate 返回 inputID 此前生效时的结果；没有 ID 或不在去重窗口内时返回 nil。
func duplicate(st *session.State, inputID string) *SubmitResult {
	ref := st.Input(inputID)
	if ref == nil {
		// 委托的任务不受去重窗口限制：窗口被子 Session 中的其他输入挤出后，重复派发也不能再提交一次。
		if p := st.Created.GetParent(); st.Delegated != "" && inputID == session.DelegatedInputID(p.GetCallId()) {
			return &SubmitResult{Duplicate: true, RunID: st.Delegated}
		}
		return nil
	}
	return &SubmitResult{Duplicate: true, RunID: ref.RunID, Steered: ref.Steered, Answered: ref.Answered}
}

// pendingQuestion 返回 Run 中等待回答的 ask_user 提问；callID 为空时返回第一个。
func pendingQuestion(r *session.Run, callID string) *session.Call {
	for _, c := range r.Calls {
		if c.Dispatched() && c.NodeID == askuser.NodeID && (callID == "" || c.Call.GetCallId() == callID) {
			return c
		}
	}
	return nil
}

// history 返回调用在本 Session 中的记录（可能已回答、已决定或随 Run 结束）：重复的回答与决定据此返回 conflict 而不是
// not found。Run 已移出投影时读日志，这条路径只在重复请求时走到。
func (s *Service) history(ctx context.Context, st *session.State, callID string) (session.CallHistory, error) {
	return s.Store.HistoryCall(ctx, st, callID, askuser.Capability)
}

// typedAnswer 把用户在对话中直接输入的内容作为对提问的回答：文字为回答文字，其余内容块（图片等）随附。
func typedAnswer(r *session.Run, c *session.Call, input []*v1.ContentBlock) (*v1.Event, error) {
	q, err := askuser.Parse(c.Call.GetArgumentsJson())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	a := &askuser.Answer{Text: strings.TrimSpace(model.Text(input))}
	var extra []*v1.ContentBlock
	for _, b := range input {
		if ca := b.GetCardAction(); ca != nil && a.Text == "" {
			// 等回答时点了卡片上的动作：动作作为回答文字。
			a.Text = model.CardActionText(ca)
			continue
		}
		if b.GetText() == nil {
			extra = append(extra, b)
		}
	}
	if a.Text == "" {
		a.Text = "（见附件）"
	}
	if err := q.Check(a); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return session.ExternalResult(r.ID, c.Call.GetCallId(), askuser.Result(q, a, extra...), false), nil
}

// Answer 记录用户在界面上对 ask_user 提问的回答（点选、填写或文字），作为该调用的结果写入日志并唤醒
// Session（ADR-0025）。回答不合法返回 ErrInvalid；提问已被回答、超时或随 Run 结束时返回 ErrConflict。
func (s *Service) Answer(ctx context.Context, sessionID, callID string, a *askuser.Answer) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	// 选项是模型给出的、已在输出时检查；用户输入的文字与填写的值按输入检查（ADR-0021）。
	if text := a.Moderated(); text != "" {
		if err := s.moderateInput(ctx, st, model.TextBlocks(text)); err != nil {
			return err
		}
	}
	return gone(sessionID, s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		r := st.Active()
		var c *session.Call
		if r != nil {
			c = pendingQuestion(r, callID)
		}
		if c == nil {
			h, err := s.history(ctx, st, callID)
			if err != nil {
				return session.Decision{}, err
			}
			if h.Asked {
				return session.Decision{}, fmt.Errorf("%w: question %s is no longer waiting for an answer", ErrConflict, callID)
			}
			return session.Decision{}, fmt.Errorf("%w: no question %s", ErrNotFound, callID)
		}
		q, err := askuser.Parse(c.Call.GetArgumentsJson())
		if err != nil {
			return session.Decision{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if err := q.Check(a); err != nil {
			return session.Decision{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		ev := session.ExternalResult(r.ID, c.Call.GetCallId(), askuser.Result(q, a), false)
		return session.Decision{Events: []*v1.Event{ev}, Wake: true}, nil
	}))
}

// Decide 记录 EndUser 对一次调用的审批决定，并唤醒 Session。
func (s *Service) Decide(ctx context.Context, sessionID, callID string, approved bool, by string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	return gone(sessionID, s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		a := st.Active()
		var call *session.Call
		if a != nil {
			for _, c := range a.Calls {
				if c.Call.GetCallId() == callID {
					call = c
				}
			}
		}
		if call == nil {
			// 已结束的 Run 中请求过审批的调用：重复的决定（如响应丢失后的重试）与 Run 进行中时一样返回 conflict，
			// 客户端不会误以为 Session 不存在（node.proto ClientRequest）。
			h, err := s.history(ctx, st, callID)
			if err != nil {
				return session.Decision{}, err
			}
			if h.ApprovalRequested {
				return session.Decision{}, fmt.Errorf("%w: call %s is not awaiting approval", ErrConflict, callID)
			}
			return session.Decision{}, fmt.Errorf("%w: no active call %s", ErrNotFound, callID)
		}
		if !call.AwaitingApproval() {
			return session.Decision{}, fmt.Errorf("%w: call %s is not awaiting approval", ErrConflict, callID)
		}
		return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_ApprovalDecided{ApprovalDecided: &v1.ApprovalDecided{
			RunId: a.ID, CallId: callID, Approved: approved, By: by,
		}}}}, Wake: true}, nil
	}))
}

// Close 关闭 Session（终态，不可逆）：中断活跃的 Run，追加 SessionClosed，由 Janitor 销毁沙箱工作区。
// 已关闭时无操作。
func (s *Service) Close(ctx context.Context, sessionID, by, reason string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	interrupted := false
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		interrupted = false
		if st.Closed != nil {
			return session.Decision{}, nil // 已关闭（如重试的 Close）：仍做一遍关闭之后的步骤
		}
		var events []*v1.Event
		a := st.Active()
		if a != nil {
			events = append(events, &v1.Event{Payload: &v1.Event_RunInterrupted{RunInterrupted: &v1.RunInterrupted{RunId: a.ID, By: by}}})
		}
		// 进行中的 Call 随之结束；中转它的进程下一次写入时发现并挂断。
		if c := st.ActiveCall; c != nil {
			events = append(events, callEnded(c.ID, CallEndSessionClosed))
		}
		events = append(events, &v1.Event{Payload: &v1.Event_SessionClosed{SessionClosed: &v1.SessionClosed{By: by, Reason: reason}}})
		interrupted = a != nil
		return session.Decision{Events: events, Wake: wakeOnEnding(st)}, nil
	})
	if err != nil {
		return gone(sessionID, err)
	}
	if interrupted {
		metrics.RunsFinished.WithLabelValues("interrupted", agentdef.Label(st.Agent)).Inc()
	}
	return s.afterClose(ctx, st)
}

// enqueueJanitor 让 Janitor 清理 Session；清理不急，一律为 Background。
func (s *Service) enqueueJanitor(ctx context.Context, sessionID, businessLine string) error {
	if s.Janitor == nil {
		return nil
	}
	return s.Janitor.Enqueue(ctx, sessionID, workqueue.Class{BusinessLine: businessLine, Priority: workqueue.Background})
}

// Delete 删除 Session：写入删除记录（立即对外不可见），由 Janitor 在后台清除全部数据。
// 已删除时无操作。requestID 非空表示属于一次按 EndUser 的删除请求。
func (s *Service) Delete(ctx context.Context, sessionID, reason, requestID string) error {
	if s.Deletions == nil {
		return fmt.Errorf("%w: deletion is not configured", ErrInvalid)
	}
	businessLine := ""
	if st, err := s.Store.Load(ctx, sessionID); err == nil && st.Created != nil {
		businessLine = st.Created.GetBusinessLine()
	} else if s.Index != nil {
		if x, err := s.Index.Get(ctx, sessionID); err == nil {
			businessLine = x.BusinessLine
		}
	}
	if businessLine == "" {
		return fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	if _, err := s.Deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: sessionID, BusinessLine: businessLine,
		Reason: reason, RequestID: requestID, RequestedAt: s.Store.Clock.Now()}); err != nil {
		return err
	}
	return s.enqueueJanitor(ctx, sessionID, businessLine)
}

// DeleteEndUser 删除 EndUser 在业务线下的全部 Session，并由 removers 删除其余用户数据（Node 登记、
// Memory 与 Grant 等），返回删除请求 ID。删除请求开始后新建的 Session 不在其中：调用方应先让该用户的令牌失效。
func (s *Service) DeleteEndUser(ctx context.Context, businessLine, endUser string, removers ...UserDataRemover) (string, error) {
	if s.Index == nil || s.Deletions == nil {
		return "", fmt.Errorf("%w: deletion is not configured", ErrInvalid)
	}
	req := &lifecycle.Request{ID: "del_" + s.Store.IDs(), BusinessLine: businessLine, CreatedAt: s.Store.Clock.Now()}
	if err := s.Deletions.CreateRequest(ctx, req); err != nil {
		return "", err
	}
	ids, err := s.Index.IDsOf(ctx, businessLine, endUser)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		if err := s.Delete(ctx, id, "account_deletion", req.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	for _, r := range removers {
		if err := r.DeleteEndUser(ctx, businessLine, endUser); err != nil {
			return "", err
		}
	}
	return req.ID, nil
}

// UserDataRemover 删除 EndUser 在业务线下的一类数据（注销账号时调用）。
type UserDataRemover interface {
	DeleteEndUser(ctx context.Context, businessLine, endUser string) error
}

// gone 把"日志已被删除"转换为不存在。
func gone(sessionID string, err error) error {
	if errors.Is(err, session.ErrGone) {
		return fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	return err
}

// WithDevice 返回注明了用户所在设备的输入（返回新切片，不修改调用方的输入）：已有界面上下文时写入其 device 字段，
// 否则在输入前加一个只含设备的界面上下文。override 为 false 时不覆盖已注明的设备。
func WithDevice(input []*v1.ContentBlock, device string, override bool) []*v1.ContentBlock {
	out := slices.Clone(input)
	for i, b := range out {
		if u := b.GetUiContext(); u != nil {
			if u.GetDevice() != "" && !override {
				return out
			}
			cp := proto.Clone(u).(*v1.UIContext)
			cp.Device = device
			out[i] = &v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: cp}}
			return out
		}
	}
	return append([]*v1.ContentBlock{{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{Device: device}}}}, out...)
}

// 界面上下文各字段的长度上限（字符）：它随每条输入进入上下文，过长会挤占窗口、稀释用户的话。
const (
	MaxUIContextScreen    = 100
	MaxUIContextRef       = 500
	MaxUIContextSelection = 2000
	MaxUIContextContent   = 8000
)

// checkUIContext 要求每条输入至多一个界面上下文、各字段不超过上限，且不能只有界面上下文而没有用户的话。
func checkUIContext(input []*v1.ContentBlock) error {
	n := 0
	for _, b := range input {
		u := b.GetUiContext()
		if u == nil {
			continue
		}
		n++
		for _, f := range []struct {
			name  string
			value string
			max   int
		}{{"screen", u.GetScreen(), MaxUIContextScreen}, {"ref", u.GetRef(), MaxUIContextRef}, {"device", u.GetDevice(), MaxUIContextScreen},
			{"selection", u.GetSelection(), MaxUIContextSelection}, {"content", u.GetContent(), MaxUIContextContent}} {
			if utf8.RuneCountInString(f.value) > f.max {
				return fmt.Errorf("%w: ui_context.%s exceeds %d characters", ErrInvalid, f.name, f.max)
			}
		}
	}
	switch {
	case n > 1:
		return fmt.Errorf("%w: at most one ui_context per input", ErrInvalid)
	case n == 1 && len(input) == 1:
		return fmt.Errorf("%w: ui_context must accompany the user's input", ErrInvalid)
	}
	return nil
}

// normalizeMedia 把输入中的 Media 规范为本 Session 工件的引用（docs/design/m2-artifacts.md）：
//   - 不接受内联的字节：Event 中只放引用，文件内容不进日志；内容安全检查的也是工件，与模型看到的一致；
//   - 不接受其他地址（http 链接等）：模型适配器会把它直接交给模型，绕过内容安全检查；
//   - 类型、大小、名称取自工件的元数据，而不是客户端自报的值。
//
// 返回新的切片，不修改调用方的输入。
func (s *Service) normalizeMedia(ctx context.Context, sessionID string, input []*v1.ContentBlock) ([]*v1.ContentBlock, error) {
	out, cloned := input, false
	for i, b := range input {
		m := b.GetMedia()
		if m == nil {
			continue
		}
		if len(m.GetData()) > 0 {
			return nil, fmt.Errorf("%w: media must reference an uploaded artifact, not carry inline data", ErrInvalid)
		}
		id, ok := artifact.ParseURI(m.GetUri())
		if !ok {
			return nil, fmt.Errorf("%w: media uri must be an artifact:// reference", ErrInvalid)
		}
		if s.Artifacts == nil {
			continue
		}
		meta, err := s.Artifacts.Stat(ctx, id)
		if errors.Is(err, artifact.ErrNotFound) || (err == nil && meta.SessionID != sessionID) {
			// 别的 Session 的工件与不存在的同样回答，不透露它是否存在。
			return nil, fmt.Errorf("%w: artifact %s not found in this session", ErrInvalid, id)
		}
		if err != nil {
			return nil, err
		}
		if !cloned {
			out, cloned = slices.Clone(input), true
		}
		out[i] = meta.Block()
	}
	return out, nil
}

// InputModerationText 是输入中需要内容安全检查的文字：用户的话，以及界面上下文——它同样由用户提交、会给模型看。
func InputModerationText(input []*v1.ContentBlock) string {
	text := model.Text(input)
	for _, b := range input {
		if a := b.GetCardAction(); a != nil {
			text += "\n" + model.CardActionText(a)
		}
		if u := b.GetUiContext(); u != nil {
			text += "\n" + strings.Join([]string{u.GetScreen(), u.GetRef(), u.GetSelection(), u.GetContent()}, "\n")
		}
	}
	return text
}

// moderateInput 检查用户输入（文本与图片工件）。违规返回 ErrRejected，服务不可用返回 ErrUnavailable：都不写日志。
func (s *Service) moderateInput(ctx context.Context, st *session.State, input []*v1.ContentBlock) error {
	ctx = faults.WithSession(ctx, st.SessionID)
	req := moderation.Request{Stage: moderation.Input, BusinessLine: st.Created.GetBusinessLine(), Text: InputModerationText(input)}
	for _, b := range input {
		if m := b.GetMedia(); m != nil && strings.HasPrefix(m.GetMimeType(), "image/") {
			id, _ := strings.CutPrefix(m.GetUri(), "artifact://")
			req.Images = append(req.Images, moderation.Image{ArtifactID: id, MimeType: m.GetMimeType()})
		}
		if m := b.GetMedia(); m != nil && strings.HasPrefix(m.GetMimeType(), "audio/") {
			id, _ := strings.CutPrefix(m.GetUri(), "artifact://")
			req.Audio = append(req.Audio, moderation.Audio{ArtifactID: id, MimeType: m.GetMimeType()})
		}
	}
	v, err := moderation.Check(ctx, s.Moderator, req)
	if err != nil {
		return err
	}
	if v.Block {
		return fmt.Errorf("%w: %s", moderation.ErrRejected, strings.Join(v.Labels, ","))
	}
	return nil
}
