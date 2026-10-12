package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/session"
)

// 后台调用（docs/design/m1-device-nodes.md §3，ADR-0035）：路由到设备的调用在设备离线超过宽限期后由 Worker 转入后台，
// Run 不再等它。之后它以三种方式之一了结，每个后台调用至多一次：设备结果（NodeResult）、到期（ExpireBackground）、
// 用户取消（CancelBackground）。前两种作为一条输入送达，由模型向用户报告；取消只记下一条说明，不开启新 Run。

// backgroundInput 返回把后台调用的了结写成一条输入的事件：有活跃 Run 时并入它（Steered），否则开启新 Run（RunRequested，
// 当前版本已撤回时同批写 AgentSwitched，ADR-0020）。活跃 Run 正在等用户回答提问时同样只是插话：结果不是用户的回答。
// 不经准入与配额检查：它不是新的交互请求，在入口拦下结果就丢了；新 Run 在调用模型之前由 Worker 的配额检查挂起。
// 设备内容不做内容安全检查，与调用结果一致。
func (s *Service) backgroundInput(st *session.State, r *v1.BackgroundResult) (events []*v1.Event, newRun bool) {
	input := []*v1.ContentBlock{{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: r}}}
	if a := st.Active(); a != nil {
		return []*v1.Event{{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: a.ID, Input: input}}}}, false
	}
	if to, ok := s.Agents.Withdrawn(st.Agent); ok {
		events = append(events, &v1.Event{Payload: &v1.Event_AgentSwitched{AgentSwitched: &v1.AgentSwitched{
			From: st.Agent, To: to.Ref(), Reason: "withdrawn"}}})
	}
	events = append(events, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{
		RunId: "run_" + s.Store.IDs(), Input: input}}})
	return events, true
}

// NodeResult 写入设备返回的路由调用结果（网关），并在同一事务中唤醒 Session。看着投影决定写什么：
//   - 调用仍在 Run 中等待：该调用的结果（ToolResult，attempt = 0）；
//   - 调用已转入后台、尚未了结：一条后台结果输入（BackgroundResult）；
//   - 否则（已有结果、已到期或已取消、Run 已终态）：结果作废，返回 session.ErrInvalid。
//
// "Worker 转入后台"与"设备结果到达"的竞争由乐观并发解决：冲突后重新决定，结果总落在其中一处，且只落一次。
// Session 已删除时返回 session.ErrGone。
func (s *Service) NodeResult(ctx context.Context, sessionID, runID, callID string, content []*v1.ContentBlock, isErr bool) error {
	st, err := s.Store.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	if st.Created == nil {
		return session.ErrGone
	}
	var prev time.Time
	settled, newRun := false, false
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		settled, newRun = false, false
		if bg := st.Background[callID]; bg != nil && st.Closed == nil {
			var events []*v1.Event
			events, newRun = s.backgroundInput(st, &v1.BackgroundResult{CallId: callID, Capability: bg.Capability,
				Content: content, IsError: isErr, Reason: session.BackgroundReasonResult})
			settled, prev = true, lastRunAt(st)
			return session.Decision{Events: events, Wake: true}, nil
		}
		return session.Decision{Events: []*v1.Event{session.ExternalResult(runID, callID, content, isErr)}, Wake: true}, nil
	})
	if err != nil || !settled {
		return err
	}
	metrics.BackgroundCalls.WithLabelValues("result").Inc()
	if !newRun {
		return nil
	}
	return s.touch(ctx, sessionID, prev)
}

// ExpireBackground 了结到截止时间仍无结果的后台调用（Worker）：写入 reason = timeout 的后台结果输入，由模型告诉用户没有办成。
// 调用方先撤回投递（Inbox），再调用它：撤回之后设备的结果不再写入，二者不会都生效。调用已了结或尚未到期时不写入。
// st 随之同步。Worker 持有 Session 的租约、随后自己推进，所以不唤醒。
func (s *Service) ExpireBackground(ctx context.Context, st *session.State, callID string) error {
	var prev time.Time
	expired, newRun := false, false
	err := s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		expired, newRun = false, false
		bg := st.Background[callID]
		if bg == nil || st.Closed != nil || s.Store.Clock.Now().Before(bg.Deadline) {
			return session.Decision{}, nil
		}
		var events []*v1.Event
		events, newRun = s.backgroundInput(st, &v1.BackgroundResult{CallId: callID, Capability: bg.Capability,
			Reason: session.BackgroundReasonTimeout})
		expired, prev = true, lastRunAt(st)
		return session.Decision{Events: events}, nil
	})
	if err != nil || !expired {
		return err
	}
	metrics.BackgroundCalls.WithLabelValues("timeout").Inc()
	if !newRun {
		return nil
	}
	return s.touch(ctx, st.SessionID, prev)
}

// CancelBackground 取消一个未了结的后台调用（用户经 HTTP 或 Connection）：先撤回投递，再写入 BackgroundCancelled。
// 不开启新 Run，不唤醒 Session：模型在之后的上下文中看到它已取消。已投递到设备的由网关发送 Cancel（尽力而为，与中断一致）。
// 调用已了结（已有结果、已到期或已取消）时返回 ErrConflict，不是后台调用时返回 ErrNotFound。
//
// 先撤回、后写入：写入失败时调用仍未了结，重试会再撤回一次；反过来，写入之后撤回之前崩溃，设备上线后仍会执行已取消的调用。
func (s *Service) CancelBackground(ctx context.Context, sessionID, callID, by string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	if bg := st.Background[callID]; bg != nil && s.Nodes != nil {
		if err := s.Nodes.Cancel(ctx, bg.NodeID, callID); err != nil {
			return err
		}
	}
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		bg := st.Background[callID]
		if bg == nil || st.Closed != nil {
			h, err := s.Store.HistoryCall(ctx, st, callID, "")
			if err != nil {
				return session.Decision{}, err
			}
			if h.Backgrounded {
				return session.Decision{}, fmt.Errorf("%w: background call %s is already settled", ErrConflict, callID)
			}
			return session.Decision{}, fmt.Errorf("%w: no background call %s", ErrNotFound, callID)
		}
		return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_BackgroundCancelled{BackgroundCancelled: &v1.BackgroundCancelled{
			CallId: callID, Capability: bg.Capability, By: by}}}}}, nil
	})
	if err != nil {
		return gone(sessionID, err)
	}
	metrics.BackgroundCalls.WithLabelValues("cancelled").Inc()
	return nil
}

// touch 在开启新 Run 之后按 TouchEvery 分段更新索引中的最近输入时间（见 submit）；prev 是此前最近一个 Run 的请求时间。
func (s *Service) touch(ctx context.Context, sessionID string, prev time.Time) error {
	now := s.Store.Clock.Now()
	if s.Index == nil || (!prev.IsZero() && now.Truncate(lifecycle.TouchEvery).Equal(prev.Truncate(lifecycle.TouchEvery))) {
		return nil
	}
	return s.Index.Touch(ctx, sessionID, now)
}

// IsDropped 报告 NodeResult 的错误是否表示结果作废（调用已不在等待、Session 已删除），而不是存储故障。
func IsDropped(err error) bool {
	return errors.Is(err, session.ErrInvalid) || errors.Is(err, session.ErrGone)
}
