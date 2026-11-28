package feed

import (
	"context"
	"errors"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/live"
	"yanshi/internal/tracing"
)

// streamTrace 从订阅者看到的 Attempt 划分增量流；一次 Attempt 可有多次生成，Delta.End 只结束其中一次。
type streamTrace struct {
	tracer  *tracing.Tracer
	span    *tracing.Span
	session string
	run     string
	attempt uint32
	seen    uint64
	from    uint64
	count   int
}

func (s *streamTrace) start(run string, attempt uint32, from uint64) {
	s.end(tracing.Done)
	s.run, s.attempt, s.from, s.count = run, attempt, from, 0
	s.span = s.tracer.Start(tracing.LiveStream, tracing.Session(s.session), tracing.Run(run), tracing.Attempt(attempt))
}

func (s *streamTrace) end(outcome tracing.Outcome) {
	if s.span != nil {
		s.span.Set(tracing.Count(tracing.Deltas, s.count))
		s.span.End(outcome)
		s.span = nil
	}
}

func (s *streamTrace) finish(err error) {
	switch {
	case err == nil:
		s.end(tracing.Done)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrDeleted):
		s.end(tracing.Canceled)
	default:
		s.end(tracing.Error)
	}
}

func (s *streamTrace) event(e *v1.Event) {
	if e.GetSeq() <= s.seen {
		return
	}
	s.seen = e.GetSeq()
	if s.seen <= s.from {
		return // 增量已先到达，其日志位置涵盖此事件。
	}
	switch p := e.GetPayload().(type) {
	case *v1.Event_AttemptStarted:
		run, attempt := p.AttemptStarted.GetRunId(), p.AttemptStarted.GetAttempt()
		if s.span == nil || run != s.run || attempt != s.attempt {
			s.start(run, attempt, s.seen)
		}
	case *v1.Event_RunSuspended:
		if p.RunSuspended.GetRunId() == s.run {
			s.end(tracing.Done)
		}
	case *v1.Event_RunCompleted:
		if p.RunCompleted.GetRunId() == s.run {
			s.end(tracing.Done)
		}
	case *v1.Event_RunFailed:
		if p.RunFailed.GetRunId() == s.run {
			s.end(tracing.Done)
		}
	case *v1.Event_RunInterrupted:
		if p.RunInterrupted.GetRunId() == s.run {
			s.end(tracing.Done)
		}
	case *v1.Event_SessionClosed:
		s.end(tracing.Done)
	}
}

// arrive 返回这条数据是否属于当前增量流。旧增量仍交给 Sink，由原有传输逻辑处理。
func (s *streamTrace) arrive(d live.Delta) bool {
	if d.End || d.RunID == "" || d.Attempt == 0 {
		return false
	}
	if s.span == nil || d.RunID != s.run || d.Attempt != s.attempt {
		if d.AfterSeq < s.seen {
			return false
		}
		s.start(d.RunID, d.Attempt, d.AfterSeq)
	}
	s.span.FirstDelta()
	return true
}
