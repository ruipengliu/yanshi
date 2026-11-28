package service

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
)

// Redo 以最后一个已完成 Run 的原输入开启新 Run，接受即替代；绝不转成插话。
func (s *Service) Redo(ctx context.Context, sessionID, inputID, expectedRunID string) (*SubmitResult, error) {
	span := s.Tracer.Start(tracing.RunSubmit, tracing.Session(sessionID))
	if expectedRunID == "" {
		return nil, fmt.Errorf("%w: expected_run_id is required", ErrInvalid)
	}
	if err := checkInputID(inputID); err != nil {
		return nil, err
	}
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var res *SubmitResult
	var prev time.Time
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		// 即使新的 Run 已失败、又被重答或 Session 已关闭，重试仍取第一次的结果。
		if res = duplicate(st, inputID); res != nil {
			return session.Decision{}, nil
		}
		if reason := st.RedoBlocker(expectedRunID); reason != "" {
			return session.Decision{}, &ReasonError{Err: fmt.Errorf("%w: redo: %s", ErrConflict, reason), Reason: reason}
		}
		original := st.Run(expectedRunID).Request
		input := make([]*v1.ContentBlock, len(original.GetInput()))
		for i, b := range original.GetInput() {
			if b != nil {
				input[i] = proto.Clone(b).(*v1.ContentBlock)
			}
		}
		// 审核位于 service 而非 RunRequested 的统一 Apply；重答仍复用该检查点，且不再次规范化原输入。
		if err := s.checkSubmitInput(ctx, st, input, true); err != nil {
			return session.Decision{}, err
		}
		request := &v1.RunRequested{InputId: inputID, Input: input, RedoOf: expectedRunID}
		events := s.newRunEvents(st, request)
		res = &SubmitResult{RunID: request.GetRunId()}
		prev = lastRunAt(st)
		return session.Decision{Events: events, Wake: true}, nil
	})
	return s.finishSubmit(ctx, st.Created.GetEndUser(), sessionID, prev, res, err, span)
}
