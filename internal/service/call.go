package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/metrics"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
	"yanshi/internal/usage"
)

// Call（全双工语音通话，docs/design/m3-call.md）在日志中的记录：开始、每句话的转写、结束。
// 媒体流不进日志（ADR-0005）；中转媒体的是 internal/call，它经这里写日志，与其他写入走同一条路径。

// CallEnded.reason 的取值。
const (
	CallEndHangup        = "hangup"
	CallEndDisconnected  = "disconnected"
	CallEndProvider      = "provider"
	CallEndReplaced      = "replaced"
	CallEndSessionClosed = "session_closed"
	CallEndQuota         = "quota"
	CallEndError         = "error"
)

// MaxCallIDLen 是客户端生成的 Call ID 的长度上限。
const MaxCallIDLen = 128

func callEnded(id, reason string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_CallEnded{CallEnded: &v1.CallEnded{CallId: id, Reason: reason}}}
}

type StartCallRequest struct {
	SessionID string
	CallID    string
	DeviceID  string
	// Model 是实时语音模型引用（provider/model），Voice 是音色。
	Model string
	Voice string
}

// CallStart 是记录下来的 Call 开始。
type CallStart struct {
	// State 是写入 CallStarted 之后的投影。中转进程从这里跟随日志：若重新加载，可能已经跨过此后的取代或关闭，
	// 永远看不到自己的结束。
	State *session.State
	// MeterID 是本次 Call 的计量标识（CallStarted 事件的 ID）：由服务端生成，客户端复用 Call ID 不会使用量
	// 因计量记录去重而丢失；也不含 Session ID（ADR-0019）。
	MeterID string
}

// StartCall 记录 Call 开始。Session 中已有进行中的 Call 时，旧的以 "replaced" 结束（同批写入）：
// 中转旧 Call 的进程可能已经崩溃，留下没有结束记录的 Call，新 Call 不能因此被永远挡住。
// 与新 Run 一样，配额用尽时拒绝（docs/design/m4-quota-usage.md §3）。
func (s *Service) StartCall(ctx context.Context, req StartCallRequest) (*CallStart, error) {
	if req.CallID == "" || len(req.CallID) > MaxCallIDLen {
		return nil, fmt.Errorf("%w: call_id is required and at most %d bytes", ErrInvalid, MaxCallIDLen)
	}
	st, err := s.Load(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	if bl := st.Created.GetBusinessLine(); s.Quotas.Enabled(bl) {
		p, err := s.Quotas.Check(ctx, bl, st.Created.GetEndUser(), s.Store.Clock.Now())
		if err != nil {
			return nil, err
		}
		if p != nil {
			metrics.QuotaRejections.WithLabelValues(p.Scope, "call").Inc()
			return nil, &usage.ExceededError{Period: p}
		}
	}
	for range maxConflictRetries {
		if st.Closed != nil {
			return nil, fmt.Errorf("%w: session %s is closed", ErrConflict, req.SessionID)
		}
		var events []*v1.Event
		if c := st.ActiveCall; c != nil {
			if c.ID == req.CallID {
				return nil, fmt.Errorf("%w: call %s already started", ErrConflict, req.CallID)
			}
			events = append(events, callEnded(c.ID, CallEndReplaced))
		}
		started := &v1.Event{Payload: &v1.Event_CallStarted{CallStarted: &v1.CallStarted{
			CallId: req.CallID, DeviceId: req.DeviceID, Model: req.Model, Voice: req.Voice}}}
		events = append(events, started)
		err := s.Store.Commit(ctx, st, events...)
		if err == nil {
			if s.Index != nil {
				if err := s.Index.Touch(ctx, req.SessionID, s.Store.Clock.Now()); err != nil {
					return nil, err
				}
			}
			return &CallStart{State: st, MeterID: started.GetId()}, nil
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return nil, err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return nil, gone(req.SessionID, err)
		}
	}
	return nil, fmt.Errorf("start call in session %s: too many conflicts", req.SessionID)
}

// AppendTranscript 写入 Call 中一句话的转写。写入前做内容安全检查（用户的话按输入、助手的话按输出，ADR-0021）：
// 违规返回 moderation.ErrRejected、服务不可用返回 ErrUnavailable，都不写日志，由调用方中止这一轮。
// Call 已不在进行中（被新 Call 取代、Session 已关闭）时返回 ErrConflict，调用方应挂断。
func (s *Service) AppendTranscript(ctx context.Context, sessionID string, t *v1.CallTranscript) error {
	if !slices.Contains(session.CallRoles, t.GetRole()) {
		return fmt.Errorf("%w: role %q", ErrInvalid, t.GetRole())
	}
	if strings.TrimSpace(t.GetText()) == "" {
		return nil
	}
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	stage := moderation.Input
	if t.GetRole() == "assistant" {
		stage = moderation.Output
	}
	v, err := moderation.Check(ctx, s.Moderator, moderation.Request{Stage: stage, BusinessLine: st.Created.GetBusinessLine(), Text: t.GetText()})
	if err != nil {
		return err
	}
	if v.Block {
		return fmt.Errorf("%w: %s", moderation.ErrRejected, strings.Join(v.Labels, ","))
	}
	for range maxConflictRetries {
		if c := st.ActiveCall; st.Closed != nil || c == nil || c.ID != t.GetCallId() {
			return fmt.Errorf("%w: call %s is not active", ErrConflict, t.GetCallId())
		}
		err := s.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_CallTranscript{CallTranscript: t}})
		if err == nil {
			return nil
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return gone(sessionID, err)
		}
	}
	return fmt.Errorf("transcript in session %s: too many conflicts", sessionID)
}

// EndCall 记录 Call 结束；Call 已结束（或被取代）时无操作。
func (s *Service) EndCall(ctx context.Context, sessionID, callID, reason string) error {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	for range maxConflictRetries {
		if c := st.ActiveCall; st.Closed != nil || c == nil || c.ID != callID {
			return nil
		}
		err := s.Store.Commit(ctx, st, callEnded(callID, reason))
		if err == nil {
			return nil
		}
		if !errors.Is(err, eventlog.ErrConflict) {
			return err
		}
		if err := s.Store.SyncAfterConflict(ctx, st); err != nil {
			return gone(sessionID, err)
		}
	}
	return fmt.Errorf("end call %s: too many conflicts", callID)
}
