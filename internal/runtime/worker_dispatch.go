package runtime

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/capability/capname"
	"yanshi/internal/delegation"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
)

// awaitDispatched 处理已派发的路由调用：超时则给出错误结果，否则（重新）投递并挂起等待。
// 重复投递是安全的：Node SDK 以 call_id 去重。
func (w *Worker) awaitDispatched(ctx context.Context, r *session.Run, c *session.Call) error {
	id := c.Call.GetCallId()
	if c.NodeID == askuser.NodeID {
		// 提问不经 Inbox（ADR-0025）：回答由用户经 Service 写入，这里只等待或超时。
		if !w.now().Before(c.Deadline) {
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks("timed out: the user did not answer the question in time"), true))
		}
		return w.suspend(ctx, r, c.Deadline)
	}
	// 设备离线时的宽限期结束时刻（ADR-0035）；设备在线、或调用不能转入后台时为零值。
	var grace time.Time
	var info *node.Info
	if w.backgroundable(c) {
		var err error
		info, err = w.cfg.Devices.Get(ctx, w.st.Created.GetEndUser(), c.NodeID)
		switch {
		case errors.Is(err, node.ErrNotFound):
			// 不在目录中（如 EndUser 已注销）：不是可以等它上线的设备，照旧等到截止时间。
		case err != nil:
			return err
		case !info.Online:
			// 宽限期从派发与设备下线中较晚者起算：刚下线的设备可能只是断网或切到后台。
			since := c.DispatchedAt
			if info.LastSeen.After(since) {
				since = info.LastSeen
			}
			grace = since.Add(w.cfg.BackgroundGrace)
		}
	}
	if grace.IsZero() {
		grace = w.delegationGrace(c) // 委托不是设备，宽限期从派发起算（ADR-0058）
	}
	// 宽限期在截止时间之前结束就转入后台，即使本步来晚了、截止时间也已过去（积压、时钟跳动）：准时醒来的 Worker 会在
	// 宽限期结束时这样做。
	if !grace.IsZero() && !w.now().Before(grace) && grace.Before(c.Deadline) {
		if delegation.IsNode(c.NodeID) {
			return w.backgroundDelegation(ctx, r, c)
		}
		return w.background(ctx, r, c, info)
	}
	if !w.now().Before(c.Deadline) {
		if err := w.cfg.Dispatch.Cancel(ctx, w.st.Created.GetEndUser(), c.NodeID, id); err != nil {
			return err
		}
		msg := "timed out: the device did not return a result in time"
		if delegation.IsNode(c.NodeID) {
			msg = "timed out: the delegated agent did not finish in time; its task was interrupted"
		}
		return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(msg), true))
	}
	until := c.Deadline
	if !grace.IsZero() {
		until = minTime(until, grace)
	}
	_, err := w.redispatch(ctx, r, c, c.Deadline, &v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: &v1.RunSuspended{
		RunId: r.ID, Attempt: w.attempt}}}, until)
	return err
}

// redispatch 以截止时间 deadline 把调用（重新）放入目标 Node 的 Inbox，再提交 e；until 非零时随后停放到 until（e 是
// RunSuspended）。重复投递是安全的：Node SDK 以 call_id 去重。
//
// 没有写入时（本 Attempt 已失效、调用已有结果或已了结、存储故障），刚才的派发可能把别人已经撤回的投递放了回去（中断、
// 取消后台调用与本步交错，或本 Worker 的投影已过时）：同步后若调用既不在 Run 中等待、也不是未了结的后台调用，就撤回它。
func (w *Worker) redispatch(ctx context.Context, r *session.Run, c *session.Call, deadline time.Time, e *v1.Event, until time.Time) (bool, error) {
	return w.deliverAndCommit(ctx, r, []delivery{{c, deadline}}, e, until)
}

// redispatchAll 以各自的截止时间（重新）派发多个调用，再提交一次 e（ADR-0055 的并行委托）。
func (w *Worker) redispatchAll(ctx context.Context, r *session.Run, calls []*session.Call, e *v1.Event, until time.Time) (bool, error) {
	return w.deliverAndCommit(ctx, r, deliveries(calls), e, until)
}

// deliveries 是已派发、尚无结果的调用，各按自己的截止时间。
func deliveries(calls []*session.Call) []delivery {
	var ds []delivery
	for _, c := range calls {
		if c.Dispatched() {
			ds = append(ds, delivery{c, c.Deadline})
		}
	}
	return ds
}

type delivery struct {
	call     *session.Call
	deadline time.Time
}

func (w *Worker) deliverAndCommit(ctx context.Context, r *session.Run, ds []delivery, e *v1.Event, until time.Time) (bool, error) {
	for _, d := range ds {
		c := d.call
		_, capName, _ := capname.CutRouted(c.Call.GetCapability())
		span := w.attemptSpan.Child(tracing.ToolCall)
		if span == nil && w.cfg.Tracer.Enabled() {
			// 挂起的并行委托可在没有新 Attempt 的情况下重投，按投影的 Attempt 号关联。
			span = w.cfg.Tracer.Start(tracing.ToolCall, tracing.Session(w.st.SessionID), tracing.Run(r.ID),
				tracing.Attempt(r.Attempt), tracing.Worker(w.cfg.ID))
		}
		if span != nil {
			kind := tracing.Node
			switch {
			case delegation.IsNode(c.NodeID):
				kind = tracing.Delegation
			case capname.IsSandboxNode(c.NodeID):
				kind = tracing.Sandbox
			}
			span.Set(tracing.Call(c.Call.GetCallId()), tracing.Kind(kind))
		}
		err := w.cfg.Dispatch.Dispatch(ctx, w.st.Created.GetEndUser(), c.NodeID, &v1.Invoke{
			SessionId: w.st.SessionID, RunId: r.ID, CallId: c.Call.GetCallId(), Capability: capName,
			ArgumentsJson: c.Call.GetArgumentsJson(), Deadline: timestamppb.New(d.deadline),
		}, w.st.WorkClass())
		if err != nil {
			span.End(model.TraceOutcome(err))
			return false, err
		}
		span.End(tracing.Dispatched)
	}
	// 提交会更新投影；先记下派发目标，失败时据此撤回。
	type target struct{ id, nodeID string }
	targets := make([]target, len(ds))
	for i, d := range ds {
		targets[i] = target{d.call.Call.GetCallId(), d.call.NodeID}
	}
	st := w.st
	// e 为 nil 时不写事件（Step 中被唤醒的父 Run 重新派发后重新停放）：派发之后照样核对，撤回已不在等待的。
	ok, err := false, error(nil)
	if e != nil {
		ok, err = w.tryCommit(ctx, e)
	}
	if !ok {
		if st != nil && w.cfg.Store.Sync(ctx, st) == nil {
			for _, t := range targets {
				id, nodeID := t.id, t.nodeID
				if waiting(st, r.ID, id) {
					continue
				}
				if cerr := w.cfg.Dispatch.Cancel(ctx, st.Created.GetEndUser(), nodeID, id); cerr != nil && err == nil {
					err = cerr
				}
			}
		}
		if e == nil && err == nil {
			return false, w.park(ctx, until)
		}
		return false, err
	}
	if !until.IsZero() {
		return true, w.park(ctx, until)
	}
	return true, nil
}

// waiting 报告调用是否仍在等待设备：在 Run 中已派发、尚无结果，或是未了结的后台调用。
func waiting(st *session.State, runID, callID string) bool {
	if st.Background[callID] != nil && st.Closed == nil {
		return true
	}
	if r := st.Run(runID); r != nil && !r.Status.Terminal() {
		for _, c := range r.Calls {
			if c.Call.GetCallId() == callID {
				return c.Dispatched()
			}
		}
	}
	return false
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
