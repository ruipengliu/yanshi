package session

import (
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
)

// ProjectionVersion 是投影语义的版本。改动 Apply 或 State 的字段时必须递增：旧快照随之作废，
// 加载退回完整回放，因此快照永远不会与当前代码的投影结果不一致。
const ProjectionVersion = 4

// Snapshots 存放每个 Session 最新的投影快照。快照只是加速加载的缓存，删除或丢失都不影响正确性。
type Snapshots interface {
	// Get 返回最新快照；没有时返回 nil。
	Get(ctx context.Context, sessionID string) (*v1.SessionSnapshot, error)
	// Put 写入快照；已有更新（seq 更大）的快照时不变。
	Put(ctx context.Context, snap *v1.SessionSnapshot) error
	Delete(ctx context.Context, sessionID string) error
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func fromTS(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

// Snapshot 返回投影的快照。事件与消息是共享的不可变对象，快照写入前会被序列化，因此无需复制。
func (s *State) Snapshot() *v1.SessionSnapshot {
	snap := &v1.SessionSnapshot{
		Version: ProjectionVersion, SessionId: s.SessionID, Seq: s.Seq, Created: s.Created, Agent: s.Agent, Closed: s.Closed,
		Compaction: s.Compaction, CompactedAt: s.CompactedAt, History: s.History,
	}
	for _, r := range s.Runs {
		rs := &v1.RunSnapshot{
			Id: r.ID, Status: int32(r.Status), RequestedAt: ts(r.RequestedAt), Attempt: r.Attempt, LiveEndpoint: r.LiveEndpoint,
			Takeovers: int32(r.Takeovers), StalledTakeovers: int32(r.StalledTakeovers), Turns: int32(r.Turns),
			Recall: r.Recall, Recalled: r.Recalled, SuspendReason: r.SuspendReason, SuspendedUntil: ts(r.SuspendedUntil),
		}
		for _, c := range r.Calls {
			cs := &v1.CallSnapshot{Call: c.Call, StartedAttempts: c.StartedAttempts, NodeId: c.NodeID, Deadline: ts(c.Deadline), Done: c.Done}
			if a := c.Approval; a != nil {
				cs.Approval = &v1.ApprovalSnapshot{Summary: a.Summary, Deadline: ts(a.Deadline), Decided: a.Decided, Approved: a.Approved}
			}
			rs.Calls = append(rs.Calls, cs)
		}
		snap.Runs = append(snap.Runs, rs)
	}
	for id := range s.callIDs {
		snap.CallIds = append(snap.CallIds, id)
	}
	slices.Sort(snap.CallIds)
	return snap
}

// FromSnapshot 由快照恢复投影；版本不符时返回错误，调用方应退回完整回放。
func FromSnapshot(snap *v1.SessionSnapshot) (*State, error) {
	if snap.GetVersion() != ProjectionVersion {
		return nil, fmt.Errorf("snapshot version %d, projection version %d", snap.GetVersion(), ProjectionVersion)
	}
	s := &State{
		SessionID: snap.GetSessionId(), Seq: snap.GetSeq(), Created: snap.GetCreated(), Agent: snap.GetAgent(), Closed: snap.GetClosed(),
		Compaction: snap.GetCompaction(), CompactedAt: snap.GetCompactedAt(), History: snap.GetHistory(),
		callIDs: map[string]bool{},
	}
	for _, rs := range snap.GetRuns() {
		r := &Run{
			ID: rs.GetId(), Status: RunStatus(rs.GetStatus()), RequestedAt: fromTS(rs.GetRequestedAt()), Attempt: rs.GetAttempt(),
			LiveEndpoint: rs.GetLiveEndpoint(), Takeovers: int(rs.GetTakeovers()), StalledTakeovers: int(rs.GetStalledTakeovers()),
			Turns: int(rs.GetTurns()), Recall: rs.GetRecall(), Recalled: rs.GetRecalled(),
			SuspendReason: rs.GetSuspendReason(), SuspendedUntil: fromTS(rs.GetSuspendedUntil()),
		}
		for _, cs := range rs.GetCalls() {
			c := &Call{Call: cs.GetCall(), StartedAttempts: cs.GetStartedAttempts(), NodeID: cs.GetNodeId(), Deadline: fromTS(cs.GetDeadline()), Done: cs.GetDone()}
			if a := cs.GetApproval(); a != nil {
				c.Approval = &Approval{Summary: a.GetSummary(), Deadline: fromTS(a.GetDeadline()), Decided: a.GetDecided(), Approved: a.GetApproved()}
			}
			r.Calls = append(r.Calls, c)
		}
		s.Runs = append(s.Runs, r)
	}
	for _, id := range snap.GetCallIds() {
		s.callIDs[id] = true
	}
	return s, nil
}

// Equal 报告两个投影是否相同（按快照的确定性序列化比较），用于测试与模拟检查快照的正确性。
func Equal(a, b *State) bool {
	opts := proto.MarshalOptions{Deterministic: true}
	x, err1 := opts.Marshal(a.Snapshot())
	y, err2 := opts.Marshal(b.Snapshot())
	return err1 == nil && err2 == nil && string(x) == string(y)
}
