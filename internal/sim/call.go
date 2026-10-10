package sim

import (
	"context"
	"errors"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/service"
)

// Call 的模拟（docs/design/m3-call.md）：模拟中转 Call 的进程经 service 写入的日志——开始（可能取代旧 Call）、
// 双方的转写、派生任务（run_task，可能以同一 ID 重试）、结束。中转进程对"哪个 Call 在进行中"的认识可能过时：
// 被新 Call 取代、Session 被关闭或删除之后，它的写入须被拒绝（ErrConflict / ErrNotFound），而不是写进日志。
// 日志的合法性由 CheckInvariants 的完整回放检查，转写的内容安全由 checkModeration 检查。

// callStep 执行一个中转进程的动作。w.calls 是各 Session 上"中转进程以为进行中"的 Call。
func (w *World) callStep() error {
	ctx := context.Background()
	sid := w.sessions[w.rng.IntN(len(w.sessions))]
	if len(w.deleted) > 0 && w.chance(0.1) {
		sid = w.deleted[w.rng.IntN(len(w.deleted))]
	}
	callID := w.calls[sid]
	if callID == "" || w.chance(0.1) {
		w.nextCall++
		id := fmt.Sprintf("vc-%d", w.nextCall)
		err := w.svc.StartCall(ctx, service.StartCallRequest{SessionID: sid, CallID: id, DeviceID: "phone", Model: "volc/sim", Voice: "v"})
		if w.callRejected(err) || w.submitOverQuota(err) {
			w.tracef("call start %s rejected: %v", sid, err)
			return nil
		}
		if err != nil {
			return fmt.Errorf("start call: %w", err)
		}
		if callID != "" {
			w.Stats.CallsReplaced++
		}
		w.Stats.CallsStarted++
		w.calls[sid] = id
		w.tracef("call start %s %s", sid, id)
		return nil
	}
	switch x := w.rng.Float64(); {
	case x < 0.55:
		role := "user"
		if w.chance(0.5) {
			role = "assistant"
		}
		t := &v1.CallTranscript{CallId: callID, Role: role, Text: fmt.Sprintf("话 %d", w.rng.IntN(1000)), Interrupted: role == "assistant" && w.chance(0.2)}
		err := w.svc.AppendTranscript(ctx, sid, t)
		if w.submitModerated(err) {
			w.Stats.CallTranscriptsBlocked++
			w.tracef("call transcript %s rejected by moderation", callID)
			return nil
		}
		if w.callRejected(err) {
			w.hangup(sid, callID, err)
			return nil
		}
		if err != nil {
			return fmt.Errorf("call transcript: %w", err)
		}
		w.Stats.CallTranscripts++
		w.tracef("call transcript %s %s", callID, role)
	case x < 0.85:
		w.nextInput++
		inputID := fmt.Sprintf("%s/t%d", callID, w.nextInput)
		input := model.TextBlocks(fmt.Sprintf("task %d", w.rng.IntN(1000)))
		res, err := w.svc.SubmitFromCall(ctx, sid, callID, inputID, input)
		if w.submitModerated(err) || w.submitOverQuota(err) {
			w.tracef("call task %s rejected: %v", callID, err)
			return nil
		}
		if w.callRejected(err) {
			w.hangup(sid, callID, err)
			return nil
		}
		if err != nil {
			return fmt.Errorf("call task: %w", err)
		}
		w.Stats.CallTasks++
		if res.Answered != "" {
			// 通话中的话回答了 Run 正在等的提问（ADR-0025 的打字回答，经语音模型转述）。
			w.Stats.CallAnswers++
		}
		w.tracef("call task %s → %s steered=%v answered=%q", callID, res.RunID, res.Steered, res.Answered)
		if w.chance(0.2) {
			// 中转进程没收到结果（如 Session 存储超时）而以同一 ID 重试。
			return w.retrySubmitFrom(sid, callID, inputID, input, res)
		}
	default:
		err := w.svc.EndCall(ctx, sid, callID, service.CallEndHangup)
		if err != nil && !errors.Is(err, service.ErrNotFound) {
			return fmt.Errorf("end call: %w", err)
		}
		delete(w.calls, sid)
		w.tracef("call end %s", callID)
	}
	return nil
}

// callRejected 报告 Call 的写入是否因 Call 不在进行中、Session 已关闭或已删除而被拒绝（预期结果）。
func (w *World) callRejected(err error) bool {
	if errors.Is(err, service.ErrConflict) || errors.Is(err, service.ErrNotFound) {
		w.Stats.CallConflicts++
		return true
	}
	return false
}

// hangup 是中转进程发现 Call 已结束后的挂断：不再写入。
func (w *World) hangup(sid, callID string, err error) {
	if w.calls[sid] == callID {
		delete(w.calls, sid)
	}
	w.tracef("call %s hung up: %v", callID, err)
}

// retrySubmitFrom 与 retrySubmit 相同，用于 Call 派生的任务。
func (w *World) retrySubmitFrom(sid, callID, inputID string, input []*v1.ContentBlock, first *service.SubmitResult) error {
	st, err := w.svc.Load(context.Background(), sid)
	if err != nil {
		return nil
	}
	res, err := w.svc.SubmitFromCall(context.Background(), sid, callID, inputID, input)
	if err != nil {
		return fmt.Errorf("retry of call task %s: %w", inputID, err)
	}
	after, err := w.svc.Load(context.Background(), sid)
	if err != nil {
		return err
	}
	if !res.Duplicate || res.RunID != first.RunID || after.Seq != st.Seq {
		return fmt.Errorf("invariant: retry of call task %s returned %+v (first %+v), log %d→%d", inputID, res, first, st.Seq, after.Seq)
	}
	w.Stats.DuplicateInputs++
	return nil
}
