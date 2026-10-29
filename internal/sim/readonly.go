package sim

import (
	"context"
	"fmt"

	"yanshi/internal/outbox"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// 独立写出预期文本，避免实现与不变量一起改变而掩盖错误。
const readOnlyChanged = "能力的声明在调用开始后发生了变化，没有执行；请重新发起调用"

type readOnlyStart struct {
	sessionID string
	runID     string
	callID    string
	attempt   uint32
}

// appendRecorded 只记录真正生效的 Started，包括生效后向 Worker 报告存储故障的情形。
func (l flakyLog) appendRecorded(ctx context.Context, sid string, expected uint64, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	head, err := l.Log.Append(ctx, sid, expected, envelopes, events...)
	if err == nil && l.w.faults && l.w.readOnlyWorker != "" {
		for _, e := range events {
			if m := e.GetToolCallStarted(); m != nil && m.GetReadOnly() {
				l.w.readOnlyStarts[l.w.readOnlyWorker] = readOnlyStart{
					sessionID: sid, runID: m.GetRunId(), callID: m.GetCallId(), attempt: m.GetAttempt(),
				}
			}
		}
	}
	return head, err
}

// beginReadOnlyDowngrade 只在两个 Step 之间改变声明，并在本步之后恢复；不会影响其他 Worker 的解析。
// 已执行待提交的调用不参与注入：它们必须保留真实结果，由 runtime 的专项测试覆盖。
func (w *World) beginReadOnlyDowngrade(workerID string) (func() error, error) {
	start, ok := w.readOnlyStarts[workerID]
	if !w.faults || !ok {
		return nil, nil
	}
	key := start.sessionID + "/" + start.callID
	if w.echoInvoked[key] != 0 {
		delete(w.readOnlyStarts, workerID)
		return nil, nil
	}
	st, err := w.store.Load(context.Background(), start.sessionID)
	if err != nil {
		return nil, err
	}
	r := st.Active()
	if r == nil || r.ID != start.runID || r.Status != session.RunRunning || r.Attempt != start.attempt {
		delete(w.readOnlyStarts, workerID)
		return nil, nil
	}
	c := r.PendingCall()
	if c == nil || c.Call.GetCallId() != start.callID || c.Call.GetCapability() != "echo" || !c.ReadOnlyInAttempt(start.attempt) {
		delete(w.readOnlyStarts, workerID)
		return nil, nil
	}
	if w.readOnlyRNG.Float64() >= 0.5 {
		return nil, nil
	}
	before, afterSeq := w.echoInvoked[key], st.Seq
	w.echo.S.ReadOnly = false
	w.tracef("read-only downgrade %s/%s attempt %d", start.sessionID, start.callID, start.attempt)
	return func() error {
		w.echo.S.ReadOnly = true
		if w.echoInvoked[key] != before {
			return fmt.Errorf("invariant: downgraded read-only call %s executed", key)
		}
		events, err := eventlog.ReadAll(context.Background(), w.log, start.sessionID, afterSeq)
		if err != nil {
			return err
		}
		for _, e := range events {
			if m := e.GetToolCallStarted(); m != nil && m.GetCallId() == start.callID && m.GetAttempt() == start.attempt {
				return fmt.Errorf("invariant: downgraded call %s rewrote Started", key)
			}
			if m := e.GetToolResult(); m != nil && m.GetRunId() == start.runID && m.GetCallId() == start.callID && m.GetAttempt() == start.attempt {
				if !m.GetIsError() || model.Text(m.GetContent()) != readOnlyChanged {
					return fmt.Errorf("invariant: downgraded call %s did not return the fixed error", key)
				}
				w.Stats.ReadOnlyDowngrades++
				delete(w.readOnlyStarts, workerID)
			}
		}
		// 续约失败、移交或追加失败可能让本步没有结果，只有真正写入固定错误才计覆盖。
		return nil
	}, nil
}
