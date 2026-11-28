package tracing

import (
	"context"
	"log/slog"
	"slices"
	"sync"
)

// Recorder 收集测试中的追踪记录，便于断言字段和父子关系。
type Recorder struct {
	slog.Handler
	mu      sync.Mutex
	records []slog.Record
}

func NewRecorder() (*Tracer, *Recorder) {
	r := &Recorder{Handler: slog.DiscardHandler}
	return New(slog.New(r)), r
}

func (*Recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *Recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec.Clone())
	return nil
}

func (r *Recorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.records)
}
