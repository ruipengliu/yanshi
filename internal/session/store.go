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
	// Waking 是与 Log 同一存储的原子"追加并入队"，供 CommitAndWake 使用；为 nil 时 CommitAndWake 报错。
	Waking eventlog.WakingLog
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
	_, err := lifecycle.Recheck(ctx, s.Deletions, st.SessionID, func(ctx context.Context) error {
		return s.Snapshots.Delete(ctx, st.SessionID)
	})
	return err
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
	return s.commit(ctx, st, false, events)
}

// ErrNoWaking 表示 Store 没有配置 Waking：CommitAndWake 不退回"先追加、再入队"两步。
var ErrNoWaking = errors.New("session: store has no waking log")

// CommitAndWake 与 Commit 相同，并在追加的同一事务中把 Session 以写入后的工作类别入队（延展性评审 §8.2）。
// 用于需要 Worker 随后推进的外部写入：输入、审批、回答、Node 结果。Worker 自己的提交不用它：Worker 提交
// RunSuspended 后紧接着 Park，此时入队会使 Park 失效。
func (s *Store) CommitAndWake(ctx context.Context, st *State, events ...*v1.Event) error {
	if s.Waking == nil {
		return ErrNoWaking
	}
	return s.commit(ctx, st, true, events)
}

func (s *Store) commit(ctx context.Context, st *State, wake bool, events []*v1.Event) error {
	now := timestamppb.New(s.Clock.Now())
	// seq 每次按当前投影重新编号：冲突后同步、以同一批事件重试时，上一次的编号已经过时。
	for i, e := range events {
		e.Id, e.SessionId, e.Seq, e.Time = s.IDs(), st.SessionID, st.Seq+uint64(i)+1, now
	}
	next, err := st.after(events...)
	if err != nil {
		return invalidError{err}
	}
	if wake {
		_, err = s.Waking.AppendAndEnqueue(ctx, st.SessionID, st.Seq, next.WorkClass(), events...)
	} else {
		_, err = s.Log.Append(ctx, st.SessionID, st.Seq, events...)
	}
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

// CallHistory 是一个调用在日志中的记录：是否经 ask_user 提问、是否请求过审批。
type CallHistory struct {
	Asked             bool
	ApprovalRequested bool
}

// historyPage 是扫描日志时每页读取的事件数。
const historyPage = 512

// ScanLog 按页读取 Session 的全部日志，逐个交给 fn；fn 返回 false 时停止。用于投影不再保留明细的历史查询
// （Run 移出投影后的调用、Janitor 的设备清除），这些路径罕见或在后台，不在活跃推进的代价之内。
func ScanLog(ctx context.Context, log eventlog.Log, sessionID string, fn func(*v1.Event) bool) error {
	for after := uint64(0); ; {
		page, err := log.Read(ctx, sessionID, after, historyPage)
		if err != nil {
			return err
		}
		for _, e := range page {
			if !fn(e) {
				return nil
			}
			after = e.GetSeq()
		}
		if len(page) < historyPage {
			return nil
		}
	}
}

// HistoryCall 在投影保留的 Run 中查找调用，找不到时扫描日志（Run 已移出投影）。
func (s *Store) HistoryCall(ctx context.Context, st *State, callID string, capability string) (CallHistory, error) {
	var h CallHistory
	found := false
	for _, r := range st.Runs {
		for _, c := range r.Calls {
			if c.Call.GetCallId() == callID {
				found = true
				h.Asked = h.Asked || c.Call.GetCapability() == capability
				h.ApprovalRequested = h.ApprovalRequested || c.Approval != nil
			}
		}
	}
	if found {
		return h, nil
	}
	err := ScanLog(ctx, s.Log, st.SessionID, func(e *v1.Event) bool {
		for _, tc := range e.GetAssistantMessage().GetToolCalls() {
			if tc.GetCallId() == callID && tc.GetCapability() == capability {
				h.Asked = true
			}
		}
		if e.GetApprovalRequested().GetCallId() == callID {
			h.ApprovalRequested = true
		}
		return e.GetSeq() < st.Seq
	})
	return h, err
}
