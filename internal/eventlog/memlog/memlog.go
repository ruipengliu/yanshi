// Package memlog 是 eventlog.Log 的进程内实现，用于单二进制模式与测试。
package memlog

import (
	"context"
	"sync"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
)

type Log struct {
	mu       sync.Mutex
	sessions map[string]*stream
}

type stream struct {
	events []*v1.Event
	// changed 在每次追加后关闭并替换，用于唤醒 Wait。
	changed chan struct{}
}

func New() *Log { return &Log{sessions: map[string]*stream{}} }

var _ eventlog.Log = (*Log)(nil)

func (l *Log) stream(id string) *stream {
	s := l.sessions[id]
	if s == nil {
		s = &stream{changed: make(chan struct{})}
		l.sessions[id] = s
	}
	return s
}

func (l *Log) Append(ctx context.Context, sessionID string, expectedSeq uint64, events ...*v1.Event) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stream(sessionID)
	if uint64(len(s.events)) != expectedSeq {
		return 0, eventlog.ErrConflict
	}
	for i, e := range events {
		e.SessionId = sessionID
		e.Seq = expectedSeq + uint64(i) + 1
		s.events = append(s.events, proto.Clone(e).(*v1.Event))
	}
	if len(events) > 0 {
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return uint64(len(s.events)), nil
}

func (l *Log) Read(ctx context.Context, sessionID string, after uint64, limit int) ([]*v1.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sessions[sessionID]
	if s == nil || after >= uint64(len(s.events)) {
		return nil, nil
	}
	out := s.events[after:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]*v1.Event(nil), out...), nil
}

func (l *Log) Wait(ctx context.Context, sessionID string, after uint64) (uint64, error) {
	for {
		l.mu.Lock()
		s := l.stream(sessionID)
		head, changed := uint64(len(s.events)), s.changed
		l.mu.Unlock()
		if head > after {
			return head, nil
		}
		select {
		case <-ctx.Done():
			return head, ctx.Err()
		case <-changed:
		}
	}
}
