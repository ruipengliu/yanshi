// Package service 是 Session 的对外操作：创建、输入、中断、查询。
// HTTP API 与模拟测试客户端都经由这里写日志，二者走同一条代码路径。
package service

import (
	"context"
	"errors"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/session"
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
	if req.AgentVersion == "" {
		def, err = s.Agents.Latest(req.Agent)
	} else {
		def, err = s.Agents.Get(&v1.AgentRef{Name: req.Agent, Version: req.AgentVersion})
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

// Load 返回 Session 的当前投影。
func (s *Service) Load(ctx context.Context, sessionID string) (*session.State, error) {
	st, err := s.Store.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if st.Created == nil {
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
	RunID string
	// Steered 为 true 表示输入并入了进行中的 Run，而非开启新 Run。
	Steered bool
}

// Submit 提交用户输入：有活跃 Run 时作为 Steer，否则开启新 Run。
func (s *Service) Submit(ctx context.Context, sessionID string, input []*v1.ContentBlock) (*SubmitResult, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrInvalid)
	}
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// 写日志前后各入队一次，多余的入队无害：
	//   - 之前：写日志后、再次入队前进程崩溃时，Run 仍有人认领；
	//   - 之后：若 Worker 在第一次入队后、写日志前认领并读到"无事可做"，它会以 done 释放并删除队列项，
	//     只有写日志之后的入队（租约期间置 dirty，或重新插入）能保证新 Run 被看到（丢失唤醒）。
	// 两者同时失效需要"恰好在该窗口内被认领"且"写日志后立即崩溃"，此时 Run 停留在 queued，
	// 直到该 Session 的下一次输入。
	if err := s.Queue.Enqueue(ctx, sessionID); err != nil {
		return nil, err
	}
	for range maxConflictRetries {
		if st.Closed != nil {
			return nil, fmt.Errorf("%w: session %s is closed", ErrConflict, sessionID)
		}
		var e *v1.Event
		res := &SubmitResult{}
		if a := st.Active(); a != nil {
			res.RunID, res.Steered = a.ID, true
			e = &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: a.ID, Input: input}}}
		} else {
			res.RunID = "run_" + s.Store.IDs()
			e = &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: res.RunID, Input: input}}}
		}
		err := s.Store.Commit(ctx, st, e)
		if err == nil {
			if s.Index != nil {
				if err := s.Index.Touch(ctx, sessionID, s.Store.Clock.Now()); err != nil {
					return res, err
				}
			}
			return res, s.Queue.Enqueue(ctx, sessionID)
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
			metrics.RunsFinished.WithLabelValues("interrupted").Inc()
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

// cancelDispatched 尽力撤回 Run 中已派发、未完成的路由调用。
func (s *Service) cancelDispatched(ctx context.Context, r *session.Run) error {
	if s.Nodes == nil {
		return nil
	}
	for _, c := range r.Calls {
		if c.Dispatched() {
			if err := s.Nodes.Cancel(ctx, c.NodeID, c.Call.GetCallId()); err != nil {
				return err
			}
		}
	}
	return nil
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
			return s.Queue.Enqueue(ctx, sessionID)
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
		events = append(events, &v1.Event{Payload: &v1.Event_SessionClosed{SessionClosed: &v1.SessionClosed{By: by, Reason: reason}}})
		err := s.Store.Commit(ctx, st, events...)
		if err == nil {
			if a != nil {
				metrics.RunsFinished.WithLabelValues("interrupted").Inc()
				if err := s.cancelDispatched(ctx, a); err != nil {
					return err
				}
			}
			if s.Index != nil {
				if err := s.Index.MarkClosed(ctx, sessionID, s.Store.Clock.Now()); err != nil {
					return err
				}
			}
			return s.enqueueJanitor(ctx, sessionID)
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

func (s *Service) enqueueJanitor(ctx context.Context, sessionID string) error {
	if s.Janitor == nil {
		return nil
	}
	return s.Janitor.Enqueue(ctx, sessionID)
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
	return s.enqueueJanitor(ctx, sessionID)
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
