package runtime

import (
	"context"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/askuser"
	"yanshi/internal/metrics"
	"yanshi/internal/notify"
	"yanshi/internal/tracing"
)

// observeCommitted 为刚提交的事件记录指标：Run 终态（时长按事件时间计算）与上下文压缩。
func (w *Worker) observeCommitted(events []*v1.Event) {
	w.notifyCommitted(events)
	w.observeAttempt()
	for _, e := range events {
		var runID, status string
		var outcome tracing.Outcome
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunCompleted:
			runID, status = p.RunCompleted.GetRunId(), "completed"
			outcome = tracing.Done
		case *v1.Event_RunFailed:
			runID, status = p.RunFailed.GetRunId(), "failed"
			outcome = tracing.Failed
		case *v1.Event_ContextCompacted:
			metrics.Compactions.WithLabelValues().Inc()
			continue
		default:
			continue
		}
		agent := agentdef.Label(w.st.Agent)
		metrics.RunsFinished.WithLabelValues(status, agent).Inc()
		if r := w.st.Run(runID); r != nil {
			metrics.RunDuration.WithLabelValues(status, agent).Observe(e.GetTime().AsTime().Sub(r.RequestedAt).Seconds())
			w.cfg.Tracer.Record(tracing.RunEnd, r.RequestedAt, e.GetTime().AsTime(), outcome,
				tracing.Session(w.st.SessionID), tracing.Run(r.ID), tracing.Attempt(r.Attempt), tracing.Worker(w.cfg.ID))
		}
	}
}

// notifyCommitted 为刚写入的、需要 EndUser 注意的事件发出提醒。只在写入时发一次：挂起后的重复检查、
// 接管后的重放都不会再发（写入后、提醒前崩溃则丢失这次提醒，提醒是尽力而为的）。
func (w *Worker) notifyCommitted(events []*v1.Event) {
	if w.cfg.Notify == nil {
		return
	}
	base := notify.Notification{BusinessLine: w.st.Created.GetBusinessLine(), EndUser: w.st.Created.GetEndUser(), SessionID: w.st.SessionID}
	for _, e := range events {
		n := base
		switch p := e.GetPayload().(type) {
		case *v1.Event_ApprovalRequested:
			n.Kind, n.RunID, n.CallID = notify.Approval, p.ApprovalRequested.GetRunId(), p.ApprovalRequested.GetCallId()
		case *v1.Event_ToolCallStarted:
			if p.ToolCallStarted.GetNodeId() != askuser.NodeID {
				continue
			}
			n.Kind, n.RunID, n.CallID = notify.Question, p.ToolCallStarted.GetRunId(), p.ToolCallStarted.GetCallId()
		case *v1.Event_RunCompleted, *v1.Event_RunFailed:
			runID := e.GetRunCompleted().GetRunId() + e.GetRunFailed().GetRunId()
			r := w.st.Run(runID)
			// 短问答不提醒：用户多半还在等着看。
			if r == nil || e.GetTime().AsTime().Sub(r.RequestedAt) < w.cfg.NotifyRunsAfter {
				continue
			}
			n.Kind, n.RunID, n.Failed = notify.RunFinished, runID, e.GetRunFailed() != nil
		default:
			continue
		}
		w.cfg.Notify.Notify(context.Background(), n)
	}
}
