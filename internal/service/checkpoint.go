package service

import (
	"context"
	"fmt"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
)

// 统一检查点（docs/design/m5-inputs-triggers-loops.md §4）：所有写进日志的入口按同一顺序检查——拒绝平台专属内容、去重、
// 规范化、内容安全、准入与配额，最后经 session.Store.Update 决定并追加。每个入口按自己的语义组合下面的步骤；
// 新的入口复用它们，而不是再写一遍。

// rejectPlatformOnly 拒绝客户端提交中只能由平台写入的内容块：后台调用的了结（ADR-0035）会被当作设备的结果送给模型；
// 旁听片段（ADR-0036）只出现在 attested 的结果中，写进输入会被当作旁听转写、按说话人决定信任。
func rejectPlatformOnly(input []*v1.ContentBlock) error {
	for _, b := range input {
		if b.GetBackgroundResult() != nil {
			return fmt.Errorf("%w: background_result is written by the platform", ErrInvalid)
		}
		if b.GetCapture() != nil {
			return fmt.Errorf("%w: capture segments are platform-only attested results", ErrInvalid)
		}
	}
	return nil
}

// moderate 在写日志之前做内容安全检查（ADR-0021）：违规返回 ErrRejected，服务不可用返回 ErrUnavailable，都不写日志。
func (s *Service) moderate(ctx context.Context, req moderation.Request) error {
	v, err := moderation.Check(ctx, s.Moderator, req)
	if err != nil {
		return err
	}
	if v.Block {
		return fmt.Errorf("%w: %s", moderation.ErrRejected, strings.Join(v.Labels, ","))
	}
	return nil
}

// admitRun 是开启新 Run 之前的准入（admission 为 false 时跳过，如委托：子 Run 是进行中的父 Run 的一部分）与配额检查。
func (s *Service) admitRun(ctx context.Context, st *session.State, admission bool) error {
	if admission && s.Admission != nil {
		if err := s.Admission.Admit(ctx, st.Created.GetBusinessLine()); err != nil {
			return err
		}
	}
	return s.CheckQuota(ctx, st, "submit")
}

// CheckClientInput 在传输层解码事件级字段后拒绝定时与重答标记；普通 Submit API 不接收这些字段。
func CheckClientInput(input []*v1.ContentBlock, mark *v1.ScheduleMark, redoOf ...string) error {
	for _, id := range redoOf {
		if id != "" {
			return fmt.Errorf("%w: redo_of is written by the platform", ErrInvalid)
		}
	}
	if mark != nil {
		return fmt.Errorf("%w: schedule mark is written by the platform", ErrInvalid)
	}
	return rejectPlatformOnly(input)
}

// checkInputID 与普通输入共享去重键的边界；空 ID 沿用无去重的既有语义。
func checkInputID(inputID string) error {
	if strings.HasPrefix(inputID, "schedule_") {
		return fmt.Errorf("%w: schedule input_id is reserved for the platform", ErrInvalid)
	}
	if len(inputID) > MaxInputIDLen {
		return fmt.Errorf("%w: input_id exceeds %d bytes", ErrInvalid, MaxInputIDLen)
	}
	return nil
}

// checkSubmitInput 共用输入审核与新 Run 的准入、配额；插话的配额由下一次模型调用检查。
func (s *Service) checkSubmitInput(ctx context.Context, st *session.State, input []*v1.ContentBlock, admission bool) error {
	if err := s.moderateInput(ctx, st, input); err != nil {
		return err
	}
	if st.Active() == nil {
		return s.admitRun(ctx, st, admission)
	}
	return nil
}

// newRunEvents 使普通输入与重答共用撤回版本切换，切换与新 Run 一起提交。
func (s *Service) newRunEvents(st *session.State, request *v1.RunRequested) []*v1.Event {
	request.RunId = "run_" + s.Store.IDs()
	var events []*v1.Event
	if to, ok := s.Agents.Withdrawn(st.Agent); ok {
		events = append(events, &v1.Event{Payload: &v1.Event_AgentSwitched{AgentSwitched: &v1.AgentSwitched{
			From: st.Agent, To: to.Ref(), Reason: "withdrawn"}}})
	}
	return append(events, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: request}})
}
