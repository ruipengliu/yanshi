package session

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
)

// Store 是读写 Session 日志的唯一入口：写入前用投影校验，写入后同步本地投影。
type Store struct {
	Log   eventlog.Log
	IDs   ids.Generator
	Clock clock.Clock
}

// Load 读取整个 Session 并投影。Session 不存在时返回 Seq 为 0 的空状态。
func (s *Store) Load(ctx context.Context, sessionID string) (*State, error) {
	st := &State{SessionID: sessionID}
	return st, s.Sync(ctx, st)
}

// Sync 把 st 之后新追加的事件应用到 st。
func (s *Store) Sync(ctx context.Context, st *State) error {
	events, err := eventlog.ReadAll(ctx, s.Log, st.SessionID, st.Seq)
	if err != nil {
		return err
	}
	for _, e := range events {
		if err := st.Apply(e); err != nil {
			return err
		}
	}
	return nil
}

// ErrGone 表示 Session 的日志已被删除（ADR-0015）：追加冲突后同步，却没有任何新事件，说明日志变短了。
var ErrGone = errors.New("session log is gone")

// SyncAfterConflict 在追加冲突后同步投影。冲突必然意味着日志前进了；若同步没有读到新事件，
// 日志只可能已被删除，返回 ErrGone，调用方应放弃而不是重试。
func (s *Store) SyncAfterConflict(ctx context.Context, st *State) error {
	before := st.Seq
	if err := s.Sync(ctx, st); err != nil {
		return err
	}
	if st.Seq == before {
		return ErrGone
	}
	return nil
}

// Commit 在 st.Seq 之后原子追加 events 并应用到 st。
// 事件违反状态机时返回校验错误；日志已前进时返回 eventlog.ErrConflict，st 不变，
// 调用方应 Sync 后重新决策。
func (s *Store) Commit(ctx context.Context, st *State, events ...*v1.Event) error {
	now := timestamppb.New(s.Clock.Now())
	for _, e := range events {
		e.Id, e.SessionId, e.Time = s.IDs(), st.SessionID, now
	}
	if err := st.Check(events...); err != nil {
		return err
	}
	if _, err := s.Log.Append(ctx, st.SessionID, st.Seq, events...); err != nil {
		return err
	}
	for _, e := range events {
		if err := st.Apply(e); err != nil {
			return err
		}
	}
	return nil
}
