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

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/askuser"
	"yanshi/internal/eventlog"
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
	Store  *session.Store
	Queue  workqueue.Queue
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

// maxConflictRetries 限制乐观并发冲突时的重试次数。
const maxConflictRetries = 16

type CreateRequest struct {
	BusinessLine string
	EndUser      string
	// Agent 是 AgentDef 名称；AgentVersion 为空时取默认版本。
	Agent        string
	AgentVersion string
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
	st := &session.State{SessionID: "ses_" + s.Store.IDs()}
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
		BusinessLine: req.BusinessLine, EndUser: req.EndUser, Agent: def.Ref(),
	}}})
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
	return s.submit(ctx, sessionID, inputID, "", input)
}

// SubmitFromCall 提交 Call 中语音模型派生的任务（run_task，docs/design/m3-call.md §5）：语义同 SubmitWithID，
// 输入带上 from_call，Run 据此把结果写成适合朗读的形式。Call 已不在进行中时返回 ErrConflict。
func (s *Service) SubmitFromCall(ctx context.Context, sessionID, callID, inputID string, input []*v1.ContentBlock) (*SubmitResult, error) {
	if callID == "" {
		return nil, fmt.Errorf("%w: call id is required", ErrInvalid)
	}
	return s.submit(ctx, sessionID, inputID, callID, input)
}

func (s *Service) submit(ctx context.Context, sessionID, inputID, fromCall string, input []*v1.ContentBlock) (*SubmitResult, error) {
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
	// 重复提交在任何检查之前返回：首次已经通过了内容安全与配额，结果也已确定。
	if res := duplicate(st, inputID); res != nil {
		return res, nil
	}
	// 写日志前后各入队一次，多余的入队无害：
	//   - 之前：写日志后、再次入队前进程崩溃时，Run 仍有人认领；
	//   - 之后：若 Worker 在第一次入队后、写日志前认领并读到"无事可做"，它会以 done 释放并删除队列项，
	//     只有写日志之后的入队（租约期间置 dirty，或重新插入）能保证新 Run 被看到（丢失唤醒）。
	// 两者同时失效需要"恰好在该窗口内被认领"且"写日志后立即崩溃"，此时 Run 停留在 queued，
	// 直到该 Session 的下一次输入。
	if err := checkUIContext(input); err != nil {
		return nil, err
	}
	if input, err = s.normalizeMedia(ctx, sessionID, input); err != nil {
		return nil, err
	}
	if err := s.moderateInput(ctx, st, input); err != nil {
		return nil, err
	}
	// 配额只拦新 Run：插话属于进行中的 Run，后者在下一次模型调用前自行检查。
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
	if err := s.Queue.Enqueue(ctx, sessionID, st.WorkClass()); err != nil {
		return nil, err
	}
	for range maxConflictRetries {
		// 并发的同 ID 提交可能刚刚生效（冲突后重读到了它）。
		if res := duplicate(st, inputID); res != nil {
			return res, nil
		}
		if st.Closed != nil {
			return nil, fmt.Errorf("%w: session %s is closed", ErrConflict, sessionID)
		}
		if fromCall != "" && (st.ActiveCall == nil || st.ActiveCall.ID != fromCall) {
			return nil, fmt.Errorf("%w: call %s is not active", ErrConflict, fromCall)
		}
		var events []*v1.Event
		res := &SubmitResult{}
		if a := st.Active(); a != nil {
			res.RunID, res.Steered = a.ID, true
			if c := pendingQuestion(a, ""); c != nil {
				ev, err := typedAnswer(a, c, input)
				if err != nil {
					return nil, err
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
		prev := lastRunAt(st)
		err := s.Store.Commit(ctx, st, events...)
		if err == nil {
			// 按 TouchEvery 分段：输入与上一个 Run 落在不同段时更新。每段的第一个输入都会写入，索引中的时间因此与
			// 最近的输入在同一段内，滞后不到 TouchEvery；只比较间隔的话，间隔始终短于 TouchEvery 的连续对话永远不会更新。
			if now := s.Store.Clock.Now(); s.Index != nil && (prev.IsZero() || !now.Truncate(lifecycle.TouchEvery).Equal(prev.Truncate(lifecycle.TouchEvery))) {
				if err := s.Index.Touch(ctx, sessionID, now); err != nil {
					return res, err
				}
			}
			return res, s.Queue.Enqueue(ctx, sessionID, st.WorkClass())
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return nil, err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return nil, gone(sessionID, err)
		}
	}
	return nil, fmt.Errorf("submit to session %s: too many conflicts", sessionID)
}

// Interrupt 中断 Run；Run 已终态时无操作并返回 nil。
func (s *Service) Interrupt(ctx context.Context, sessionID, runID string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	for range maxConflictRetries {
		r := st.Run(runID)
		if r == nil {
			return fmt.Errorf("%w: run %s", ErrNotFound, runID)
		}
		if r.Status.Terminal() {
			return nil
		}
		err := s.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_RunInterrupted{
			RunInterrupted: &v1.RunInterrupted{RunId: runID, By: "user"},
		}})
		if err == nil {
			metrics.RunsFinished.WithLabelValues("interrupted", agentdef.Label(st.Agent)).Inc()
			return s.cancelDispatched(ctx, r)
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return gone(sessionID, err)
		}
	}
	return fmt.Errorf("interrupt run %s: too many conflicts", runID)
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

// askedBefore 报告 callID 是否是本 Session 中的一次 ask_user 提问（可能已回答或已结束）。
func askedBefore(st *session.State, callID string) bool {
	for _, r := range st.Runs {
		for _, c := range r.Calls {
			if c.Call.GetCallId() == callID && c.Call.GetCapability() == askuser.Capability {
				return true
			}
		}
	}
	return false
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
	return answerEvent(r, c, askuser.Result(q, a, extra...)), nil
}

// answerEvent 是回答对应的外部结果（attempt = 0）。
func answerEvent(r *session.Run, c *session.Call, content []*v1.ContentBlock) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: r.ID, CallId: c.Call.GetCallId(), Content: content}}}
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
	for range maxConflictRetries {
		r := st.Active()
		var c *session.Call
		if r != nil {
			c = pendingQuestion(r, callID)
		}
		if c == nil {
			if askedBefore(st, callID) {
				return fmt.Errorf("%w: question %s is no longer waiting for an answer", ErrConflict, callID)
			}
			return fmt.Errorf("%w: no question %s", ErrNotFound, callID)
		}
		q, err := askuser.Parse(c.Call.GetArgumentsJson())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if err := q.Check(a); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		err = s.Store.Commit(ctx, st, answerEvent(r, c, askuser.Result(q, a)))
		if err == nil {
			return s.Queue.Enqueue(ctx, sessionID, st.WorkClass())
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return gone(sessionID, err)
		}
	}
	return fmt.Errorf("answer question %s: too many conflicts", callID)
}

// Decide 记录 EndUser 对一次调用的审批决定，并唤醒 Session。
func (s *Service) Decide(ctx context.Context, sessionID, callID string, approved bool, by string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	for range maxConflictRetries {
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
			return fmt.Errorf("%w: no active call %s", ErrNotFound, callID)
		}
		if !call.AwaitingApproval() {
			return fmt.Errorf("%w: call %s is not awaiting approval", ErrConflict, callID)
		}
		err := s.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ApprovalDecided{ApprovalDecided: &v1.ApprovalDecided{
			RunId: a.ID, CallId: callID, Approved: approved, By: by,
		}}})
		if err == nil {
			return s.Queue.Enqueue(ctx, sessionID, st.WorkClass())
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return gone(sessionID, err)
		}
	}
	return fmt.Errorf("decide call %s: too many conflicts", callID)
}

// Close 关闭 Session（终态，不可逆）：中断活跃的 Run，追加 SessionClosed，由 Janitor 销毁沙箱工作区。
// 已关闭时无操作。
func (s *Service) Close(ctx context.Context, sessionID, by, reason string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	for range maxConflictRetries {
		if st.Closed != nil {
			return nil
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
		err := s.Store.Commit(ctx, st, events...)
		if err == nil {
			if a != nil {
				metrics.RunsFinished.WithLabelValues("interrupted", agentdef.Label(st.Agent)).Inc()
				if err := s.cancelDispatched(ctx, a); err != nil {
					return err
				}
			}
			if s.Index != nil {
				if err := s.Index.MarkClosed(ctx, sessionID, s.Store.Clock.Now()); err != nil {
					return err
				}
			}
			return s.enqueueJanitor(ctx, sessionID, st.Created.GetBusinessLine())
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return gone(sessionID, err)
		}
	}
	return fmt.Errorf("close session %s: too many conflicts", sessionID)
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
		}{{"screen", u.GetScreen(), MaxUIContextScreen}, {"ref", u.GetRef(), MaxUIContextRef},
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
		if u := b.GetUiContext(); u != nil {
			text += "\n" + strings.Join([]string{u.GetScreen(), u.GetRef(), u.GetSelection(), u.GetContent()}, "\n")
		}
	}
	return text
}

// moderateInput 检查用户输入（文本与图片工件）。违规返回 ErrRejected，服务不可用返回 ErrUnavailable：都不写日志。
func (s *Service) moderateInput(ctx context.Context, st *session.State, input []*v1.ContentBlock) error {
	req := moderation.Request{Stage: moderation.Input, BusinessLine: st.Created.GetBusinessLine(), Text: InputModerationText(input)}
	for _, b := range input {
		if m := b.GetMedia(); m != nil && strings.HasPrefix(m.GetMimeType(), "image/") {
			id, _ := strings.CutPrefix(m.GetUri(), "artifact://")
			req.Images = append(req.Images, moderation.Image{ArtifactID: id, MimeType: m.GetMimeType()})
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
