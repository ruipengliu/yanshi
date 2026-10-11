package session

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
)

// Store 是读写 Session 日志的唯一入口：写入前用投影校验，写入后同步本地投影。
type Store struct {
	Log   eventlog.Log
	IDs   ids.Generator
	Clock clock.Clock

	// Snapshots 非空时，Load 从最新快照加上其后的日志恢复投影；加载时若快照之后的事件
	// 超过 SnapshotEvery（默认 200）条，就写入新快照。
	Snapshots     Snapshots
	SnapshotEvery int
	// Deletions 非空时，写入快照后复查删除记录，Session 已删除则撤回（ADR-0015）。
	Deletions lifecycle.Deletions
}

// Load 读取 Session 并投影。Session 不存在时返回 Seq 为 0 的空状态。
func (s *Store) Load(ctx context.Context, sessionID string) (*State, error) {
	st, err := s.fromSnapshot(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	base := st.Seq
	if err := s.Sync(ctx, st); err != nil {
		return nil, err
	}
	metrics.SessionLoadEvents.WithLabelValues().Observe(float64(st.Seq - base))
	every := s.SnapshotEvery
	if every <= 0 {
		every = 200
	}
	if s.Snapshots != nil && st.Created != nil && st.Seq-base >= uint64(every) {
		if err := s.putSnapshot(ctx, st); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// fromSnapshot 返回由快照恢复的投影，没有可用快照时返回空投影。快照对应的事件必须仍在日志中：
// 日志被删除（ADR-0015）后残留的快照不能让 Session "复活"。
func (s *Store) fromSnapshot(ctx context.Context, sessionID string) (*State, error) {
	empty := &State{SessionID: sessionID}
	if s.Snapshots == nil {
		return empty, nil
	}
	snap, err := s.Snapshots.Get(ctx, sessionID)
	if err != nil || snap == nil || snap.GetSeq() == 0 {
		return empty, err
	}
	at, err := s.Log.Read(ctx, sessionID, snap.GetSeq()-1, 1)
	if err != nil {
		return nil, err
	}
	if len(at) == 0 {
		return empty, nil
	}
	st, err := FromSnapshot(snap)
	if err != nil {
		return empty, nil // 投影版本已变化：完整回放
	}
	return st, nil
}

func (s *Store) putSnapshot(ctx context.Context, st *State) error {
	if err := s.Snapshots.Put(ctx, st.Snapshot()); err != nil {
		return err
	}
	if s.Deletions != nil {
		if gone, err := lifecycle.Deleted(ctx, s.Deletions, st.SessionID); err != nil || gone {
			if gone {
				return s.Snapshots.Delete(ctx, st.SessionID)
			}
			return err
		}
	}
	return nil
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

// ErrInvalid 表示事件违反状态机（如调用已有结果、Run 已终态）：与存储故障不同，同样的写入重试也不会成功。
// Commit 返回的校验错误满足 errors.Is(err, ErrInvalid)，错误文本不变。
var ErrInvalid = errors.New("invalid event")

type invalidError struct{ err error }

func (e invalidError) Error() string   { return e.err.Error() }
func (e invalidError) Unwrap() []error { return []error{ErrInvalid, e.err} }

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
// 事件违反状态机时返回校验错误（ErrInvalid）；日志已前进时返回 eventlog.ErrConflict，st 不变，
// 调用方应 Sync 后重新决策。
func (s *Store) Commit(ctx context.Context, st *State, events ...*v1.Event) error {
	now := timestamppb.New(s.Clock.Now())
	// seq 每次按当前投影重新编号：冲突后同步、以同一批事件重试时，上一次的编号已经过时。
	for i, e := range events {
		e.Id, e.SessionId, e.Seq, e.Time = s.IDs(), st.SessionID, st.Seq+uint64(i)+1, now
	}
	if err := st.Check(events...); err != nil {
		return invalidError{err}
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
