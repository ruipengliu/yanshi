package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/metrics"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
)

func (w *Worker) owns(r *session.Run) bool {
	return r.ID == w.runID && r.Attempt == w.attempt && r.Status == session.RunRunning
}

// commit 以本 Worker 的 Attempt 追加事件；冲突时同步后若仍持有 Attempt 则重试。
// 若 Attempt 已失效（Run 被中断、接管或已终态），静默放弃。
func (w *Worker) commit(ctx context.Context, events ...*v1.Event) error {
	_, err := w.tryCommit(ctx, events...)
	return err
}

// tryCommit 同 commit，并报告事件是否已写入。
func (w *Worker) tryCommit(ctx context.Context, events ...*v1.Event) (bool, error) {
	// written 记录最后一次决策是否给出了事件：Update 返回 nil 时，给出事件的那次决策已提交，
	// 不给事件（Attempt 已失效）则什么也没写。每次提交前都检查 Attempt，冲突后同步到的中断或接管随即生效。
	written := false
	err := w.cfg.Store.Update(ctx, w.st, func(st *session.State) (session.Decision, error) {
		r := st.Run(w.runID)
		if r == nil || !w.owns(r) || settledMeanwhile(r, events) {
			written = false
			return session.Decision{}, nil
		}
		written = true
		return session.Decision{Events: events}, nil
	})
	if errors.Is(err, session.ErrGone) {
		// Session 已删除：放弃并丢弃全部本地状态。否则本地投影仍认为调用"已由我开始、尚无结果"，
		// 而按需续约又可能尚未发现租约已被移除，下一步会再次执行非幂等调用（模拟测试发现）。
		w.drop()
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !written {
		w.observeAttempt()
		return false, nil
	}
	w.observeCommitted(events)
	return true, nil
}

// settledMeanwhile 报告 events 中本 Worker 给出的调用结果（超时、转入后台的占位）所针对的路由调用是否已在冲突后同步到的
// 投影中有了结果：设备或子 Session 的结果与本步的决定同时到达（ADR-0058 的转入后台与子 Run 写回结果竞争）。这时不写，
// 与 Attempt 失效一样什么也不提交，下一步按新的投影重新决定。
func settledMeanwhile(r *session.Run, events []*v1.Event) bool {
	for _, e := range events {
		res := e.GetToolResult()
		if res == nil || res.GetAttempt() == 0 {
			continue
		}
		for _, c := range r.Calls {
			if c.Call.GetCallId() == res.GetCallId() && c.Done && c.NodeID != "" {
				return true
			}
		}
	}
	return false
}

// suspend 追加 events 与 RunSuspended，并把 Session 停放到 until；之后本 Worker 不再持有该 Run。
func (w *Worker) suspend(ctx context.Context, r *session.Run, until time.Time, events ...*v1.Event) error {
	return w.suspendWith(ctx, &v1.RunSuspended{RunId: r.ID, Attempt: w.attempt}, until, events...)
}

func (w *Worker) suspendWith(ctx context.Context, s *v1.RunSuspended, until time.Time, events ...*v1.Event) error {
	events = append(events, &v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: s}})
	ok, err := w.tryCommit(ctx, events...)
	if err != nil || !ok {
		return err
	}
	return w.park(ctx, until)
}

func (w *Worker) startAttempt(ctx context.Context, r *session.Run) error {
	if r.Status == session.RunRunning && r.StalledTakeovers >= w.cfg.MaxTakeovers {
		return w.fail(ctx, r, fmt.Sprintf("exceeded %d takeovers without progress", w.cfg.MaxTakeovers))
	}
	next := r.Attempt + 1
	span := w.cfg.Tracer.Start(tracing.RunAttempt,
		tracing.Session(w.st.SessionID), tracing.Run(r.ID), tracing.Attempt(next), tracing.Worker(w.cfg.ID))
	// 提交前确定 Attempt 的类型：提交后投影中的状态已是 running。
	kind := map[session.RunStatus]string{session.RunQueued: "start", session.RunRunning: "takeover", session.RunSuspended: "resume"}[r.Status]
	err := w.cfg.Store.Commit(ctx, w.st, &v1.Event{Payload: &v1.Event_AttemptStarted{
		AttemptStarted: &v1.AttemptStarted{RunId: r.ID, Attempt: next, WorkerId: w.cfg.ID, LiveEndpoint: w.cfg.LiveEndpoint},
	}})
	if errors.Is(err, eventlog.ErrConflict) {
		return nil // 下一步 Sync 后重新决策
	}
	if err != nil {
		return err
	}
	w.runID, w.attempt, w.modelErrors, w.forceCompact = r.ID, next, 0, false
	w.attemptSpan = span
	w.executed = nil
	metrics.Attempts.WithLabelValues(kind).Inc()
	w.cfg.Logger.Info("attempt started", "worker", w.cfg.ID, "session", w.st.SessionID, "run", r.ID, "attempt", next)
	return nil
}

func (w *Worker) endAttempt(outcome tracing.Outcome) {
	w.attemptSpan.End(outcome)
	w.attemptSpan = nil
}

// observeAttempt 以提交或同步后的投影结束本地记录。若投影已进入下一次 Attempt，旧 Worker 只知道自己失去了所有权。
func (w *Worker) observeAttempt() {
	if w.attemptSpan == nil {
		return
	}
	r := w.st.Run(w.runID)
	if r == nil || r.Attempt != w.attempt {
		w.endAttempt(tracing.Lost)
		return
	}
	switch r.Status {
	case session.RunCompleted:
		w.endAttempt(tracing.Done)
	case session.RunFailed:
		w.endAttempt(tracing.Failed)
	case session.RunInterrupted:
		w.endAttempt(tracing.Interrupted)
	case session.RunSuspended:
		if r.SuspendReason == session.SuspendHandoff {
			w.endAttempt(tracing.Handoff)
		} else {
			w.endAttempt(tracing.Suspended)
		}
	}
}

func (w *Worker) fail(ctx context.Context, r *session.Run, reason string) error {
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	w.cfg.Logger.Warn("run failed", "session", w.st.SessionID, "run", r.ID, "reason", reason)
	e := &v1.Event{Payload: &v1.Event_RunFailed{RunFailed: &v1.RunFailed{RunId: r.ID, Attempt: r.Attempt, Reason: reason}}}
	err := w.cfg.Store.Commit(ctx, w.st, e)
	if errors.Is(err, session.ErrGone) {
		// RunFailed 的信封复查也可能先于日志清理发现删除；与 tryCommit 一样终止并清空本地状态。
		w.drop()
		return nil
	}
	if errors.Is(err, eventlog.ErrConflict) {
		return nil
	}
	if err == nil {
		w.observeCommitted([]*v1.Event{e})
	}
	return err
}

var errSuperseded = errors.New("attempt superseded")

// during 执行耗时操作 fn。期间若本 Attempt 被中断或接管，取消 fn 并返回 errSuperseded；
// 若配置了 Heartbeat，则期间持续续约，续约失败同样取消 fn。
func (w *Worker) during(ctx context.Context, r *session.Run, fn func(context.Context) error) error {
	opCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go w.watchSupersede(opCtx, cancel, w.st.SessionID, r.ID, w.st.Seq)
	if w.cfg.Heartbeat > 0 {
		go func() {
			t := time.NewTicker(w.cfg.Heartbeat)
			defer t.Stop()
			for {
				select {
				case <-opCtx.Done():
					return
				case <-t.C:
					if err := w.cfg.Queue.Renew(opCtx, w.lease, w.cfg.LeaseTTL); err != nil {
						cancel(errSuperseded)
						return
					}
				}
			}
		}()
	}

	err := fn(opCtx)
	if errors.Is(context.Cause(opCtx), errSuperseded) {
		return errSuperseded
	}
	return err
}

// watchSupersede 监视日志，一旦本 Run 终态或出现新的 Attempt 即取消当前操作。
func (w *Worker) watchSupersede(ctx context.Context, cancel context.CancelCauseFunc, sid, runID string, after uint64) {
	for {
		if _, err := w.cfg.Store.Log.Wait(ctx, sid, after); err != nil {
			return
		}
		events, err := eventlog.ReadAll(ctx, w.cfg.Store.Log, sid, after)
		if err != nil {
			return
		}
		for _, e := range events {
			after = e.GetSeq()
			switch p := e.GetPayload().(type) {
			case *v1.Event_RunInterrupted:
				if p.RunInterrupted.GetRunId() == runID {
					cancel(errSuperseded)
					return
				}
			case *v1.Event_AttemptStarted:
				if p.AttemptStarted.GetRunId() == runID {
					cancel(errSuperseded)
					return
				}
			}
		}
	}
}
