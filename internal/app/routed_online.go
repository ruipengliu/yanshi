package app

import (
	"context"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/node"
	"yanshi/internal/notify"
	"yanshi/internal/outbox"
	"yanshi/internal/presence"
	"yanshi/internal/sandbox"
	"yanshi/internal/session"
	"yanshi/internal/shard"
	"yanshi/internal/usage"
	"yanshi/sdk/nodesdk"
)

// 在线方法只根据真实键选一次分片；底层实现继续负责归属校验。

type routedLog struct{ r *storeRouter }

var _ eventlog.Log = routedLog{}

func (x routedLog) Append(ctx context.Context, sessionID string, expectedSeq uint64, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return 0, err
	}
	return s.Log.Append(ctx, sessionID, expectedSeq, envelopes, events...)
}

func (x routedLog) Read(ctx context.Context, sessionID string, after uint64, limit int) ([]*v1.Event, error) {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return nil, err
	}
	return s.Log.Read(ctx, sessionID, after, limit)
}

func (x routedLog) Wait(ctx context.Context, sessionID string, after uint64) (uint64, error) {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return 0, err
	}
	return s.Log.Wait(ctx, sessionID, after)
}

func (x routedLog) Delete(ctx context.Context, sessionID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Log.Delete(ctx, sessionID)
}

type routedDir struct{ r *storeRouter }

var _ node.Directory = routedDir{}

func (x routedDir) SetOffline(ctx context.Context, endUser, nodeID string, gen uint64) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Dir.SetOffline(ctx, endUser, nodeID, gen)
}

func (x routedDir) Get(ctx context.Context, endUser, nodeID string) (*node.Info, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Dir.Get(ctx, endUser, nodeID)
}

func (x routedDir) List(ctx context.Context, scope node.Scope) ([]*node.Info, error) {
	s, err := x.r.pick(ctx, shard.EndUser, scope.EndUser)
	if err != nil {
		return nil, err
	}
	return s.Dir.List(ctx, scope)
}

func (x routedDir) DeleteScope(ctx context.Context, scope node.Scope) ([]string, error) {
	s, err := x.r.pick(ctx, shard.EndUser, scope.EndUser)
	if err != nil {
		return nil, err
	}
	return s.Dir.DeleteScope(ctx, scope)
}

func (x routedDir) Register(ctx context.Context, info node.Info) (string, uint64, error) {
	s, err := x.r.pick(ctx, shard.EndUser, info.Scope.EndUser)
	if err != nil {
		return "", 0, err
	}
	return s.Dir.Register(ctx, info)
}

type routedInbox struct{ r *storeRouter }

var _ node.Inbox = routedInbox{}

func (x routedInbox) Put(ctx context.Context, endUser, nodeID string, inv *v1.Invoke) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Inbox.Put(ctx, endUser, nodeID, inv)
}

func (x routedInbox) Remove(ctx context.Context, endUser, nodeID, callID string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Inbox.Remove(ctx, endUser, nodeID, callID)
}

func (x routedInbox) Pending(ctx context.Context, endUser, nodeID string) ([]*v1.Invoke, uint64, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, 0, err
	}
	return s.Inbox.Pending(ctx, endUser, nodeID)
}

func (x routedInbox) Wait(ctx context.Context, endUser, nodeID string, version uint64) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Inbox.Wait(ctx, endUser, nodeID, version)
}

type routedLedger struct{ r *storeRouter }

var _ sandbox.Ledger = routedLedger{}

func (x routedLedger) Get(sessionID, callID string) (*nodesdk.Record, error) {
	s, err := x.r.pick(context.Background(), shard.SessionData, sessionID)
	if err != nil {
		return nil, err
	}
	return s.Ledger.Get(sessionID, callID)
}

func (x routedLedger) Put(sessionID, callID string, record *nodesdk.Record) error {
	s, err := x.r.pick(context.Background(), shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Ledger.Put(sessionID, callID, record)
}

func (x routedLedger) Forget(sessionID string) error {
	s, err := x.r.pick(context.Background(), shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Ledger.Forget(sessionID)
}

func (x routedLedger) HasSession(sessionID string) (bool, error) {
	s, err := x.r.pick(context.Background(), shard.SessionData, sessionID)
	if err != nil {
		return false, err
	}
	return s.Ledger.HasSession(sessionID)
}

type routedActivity struct{ r *storeRouter }

var _ sandbox.Activity = routedActivity{}

func (x routedActivity) Touch(ctx context.Context, sessionID string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.SessionData, strings.TrimPrefix(sessionID, sandbox.NodePrefix))
	if err != nil {
		return err
	}
	return s.Activity.Touch(ctx, sessionID, at)
}

func (x routedActivity) Delete(ctx context.Context, sessionID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, strings.TrimPrefix(sessionID, sandbox.NodePrefix))
	if err != nil {
		return err
	}
	return s.Activity.Delete(ctx, sessionID)
}

type routedArtifactMeta struct{ r *storeRouter }

var _ artifact.MetaStore = routedArtifactMeta{}

func (x routedArtifactMeta) Get(ctx context.Context, sessionID, id string) (*artifact.Meta, error) {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return nil, err
	}
	return s.ArtifactMeta.Get(ctx, sessionID, id)
}

func (x routedArtifactMeta) List(ctx context.Context, sessionID string) ([]*artifact.Meta, error) {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return nil, err
	}
	return s.ArtifactMeta.List(ctx, sessionID)
}

func (x routedArtifactMeta) Delete(ctx context.Context, sessionID, id string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.ArtifactMeta.Delete(ctx, sessionID, id)
}

func (x routedArtifactMeta) Create(ctx context.Context, m *artifact.Meta) error {
	s, err := x.r.pick(ctx, shard.SessionData, m.SessionID)
	if err != nil {
		return err
	}
	return s.ArtifactMeta.Create(ctx, m)
}

func (x routedArtifactMeta) Lookup(ctx context.Context, id string) (*artifact.Meta, error) {
	s, err := x.r.pick(ctx, shard.Object, id)
	if err != nil {
		return nil, err
	}
	return s.ArtifactMeta.Lookup(ctx, id)
}

type routedIndex struct{ r *storeRouter }

var _ lifecycle.Index = routedIndex{}

func (x routedIndex) Put(ctx context.Context, entry *lifecycle.Session) error {
	s, err := x.r.pick(ctx, shard.EndUser, entry.EndUser)
	if err != nil {
		return err
	}
	return s.Index.Put(ctx, entry)
}

func (x routedIndex) LookupSession(ctx context.Context, id string) (*lifecycle.Session, error) {
	s, err := x.r.pick(ctx, shard.SessionIndex, id)
	if err != nil {
		return nil, err
	}
	return s.Index.LookupSession(ctx, id)
}

func (x routedIndex) Get(ctx context.Context, endUser, id string) (*lifecycle.Session, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Index.Get(ctx, endUser, id)
}

func (x routedIndex) Delete(ctx context.Context, endUser, id string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Index.Delete(ctx, endUser, id)
}

func (x routedIndex) ListStable(ctx context.Context, businessLine, endUser, cursor string, limit int) ([]*lifecycle.Session, string, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, "", err
	}
	return s.Index.ListStable(ctx, businessLine, endUser, cursor, limit)
}

func (x routedIndex) IDsOf(ctx context.Context, businessLine, endUser string) ([]string, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Index.IDsOf(ctx, businessLine, endUser)
}

func (x routedIndex) Touch(ctx context.Context, endUser, id string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Index.Touch(ctx, endUser, id, at)
}

func (x routedIndex) SetNextDue(ctx context.Context, endUser, id string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Index.SetNextDue(ctx, endUser, id, at)
}

func (x routedIndex) MarkClosed(ctx context.Context, endUser, id string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Index.MarkClosed(ctx, endUser, id, at)
}

type routedDeletions struct{ r *storeRouter }

var _ lifecycle.Deletions = routedDeletions{}

func (x routedDeletions) Get(ctx context.Context, sessionID string) (*lifecycle.Tombstone, error) {
	// 空 ID 不可能有合法删除记录；保留不存在的读语义，不把无效键送去任一分片。
	if sessionID == "" {
		return nil, lifecycle.ErrNotFound
	}
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return nil, err
	}
	return s.Deletions.Get(ctx, sessionID)
}

func (x routedDeletions) Complete(ctx context.Context, sessionID string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Deletions.Complete(ctx, sessionID, at)
}

func (x routedDeletions) Mark(ctx context.Context, t *lifecycle.Tombstone) (bool, error) {
	s, err := x.r.pick(ctx, shard.SessionData, t.SessionID)
	if err != nil {
		return false, err
	}
	return s.Deletions.Mark(ctx, t)
}

func (x routedDeletions) GetEndUser(ctx context.Context, businessLine, endUser string) (*lifecycle.EndUserTombstone, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Deletions.GetEndUser(ctx, businessLine, endUser)
}

func (x routedDeletions) MarkEndUser(ctx context.Context, b *lifecycle.EndUserTombstone) (*lifecycle.EndUserTombstone, error) {
	s, err := x.r.pick(ctx, shard.EndUser, b.EndUser)
	if err != nil {
		return nil, err
	}
	return s.Deletions.MarkEndUser(ctx, b)
}

func (x routedDeletions) SealEndUser(ctx context.Context, b *lifecycle.EndUserTombstone) error {
	s, err := x.r.pick(ctx, shard.EndUser, b.EndUser)
	if err != nil {
		return err
	}
	return s.Deletions.SealEndUser(ctx, b)
}

func (x routedDeletions) AddEndUserSessions(ctx context.Context, b *lifecycle.EndUserTombstone, ids []string) error {
	s, err := x.r.pick(ctx, shard.EndUser, b.EndUser)
	if err != nil {
		return err
	}
	return s.Deletions.AddEndUserSessions(ctx, b, ids)
}

func (x routedDeletions) CompleteEndUser(ctx context.Context, b *lifecycle.EndUserTombstone, at, expires time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, b.EndUser)
	if err != nil {
		return err
	}
	return s.Deletions.CompleteEndUser(ctx, b, at, expires)
}

type routedMemories struct{ r *storeRouter }

var _ memory.Store = routedMemories{}

func (x routedMemories) Get(ctx context.Context, endUser, id string) (*memory.Memory, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Memories.Get(ctx, endUser, id)
}

func (x routedMemories) Delete(ctx context.Context, endUser, id string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Memories.Delete(ctx, endUser, id)
}

func (x routedMemories) Count(ctx context.Context, businessLine, endUser string, at time.Time) (int, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return 0, err
	}
	return s.Memories.Count(ctx, businessLine, endUser, at)
}

func (x routedMemories) List(ctx context.Context, endUser string, scopes []memory.Scope, at time.Time, limit int) ([]*memory.Memory, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Memories.List(ctx, endUser, scopes, at, limit)
}

func (x routedMemories) ListBySource(ctx context.Context, businessLine, endUser string, sessions []string, at time.Time) ([]*memory.Memory, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Memories.ListBySource(ctx, businessLine, endUser, sessions, at)
}

func (x routedMemories) Nearest(ctx context.Context, endUser string, scopes []memory.Scope, at time.Time, vec memory.Vector, limit int) ([]memory.Hit, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Memories.Nearest(ctx, endUser, scopes, at, vec, limit)
}

func (x routedMemories) Supersede(ctx context.Context, endUser, id, by string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Memories.Supersede(ctx, endUser, id, by, at)
}

func (x routedMemories) Purge(ctx context.Context, businessLine, endUser string, supersededBefore, expiredBefore time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Memories.Purge(ctx, businessLine, endUser, supersededBefore, expiredBefore)
}

func (x routedMemories) DeleteSessionForEndUser(ctx context.Context, endUser, sessionID string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Memories.DeleteSessionForEndUser(ctx, endUser, sessionID)
}

func (x routedMemories) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Memories.DeleteEndUser(ctx, businessLine, endUser)
}

func (x routedMemories) Accesses(ctx context.Context, endUser, owner string, since time.Time, limit int) ([]memory.Access, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Memories.Accesses(ctx, endUser, owner, since, limit)
}

func (x routedMemories) Put(ctx context.Context, m *memory.Memory, vec *memory.Vector) error {
	s, err := x.r.pick(ctx, shard.EndUser, m.EndUser)
	if err != nil {
		return err
	}
	return s.Memories.Put(ctx, m, vec)
}

type routedGrants struct{ r *storeRouter }

var _ memory.Grants = routedGrants{}

func (x routedGrants) Get(ctx context.Context, endUser, id string) (*memory.Grant, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Grants.Get(ctx, endUser, id)
}

func (x routedGrants) Revoke(ctx context.Context, endUser, id string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Grants.Revoke(ctx, endUser, id, at)
}

func (x routedGrants) List(ctx context.Context, endUser, businessLine string) ([]*memory.Grant, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Grants.List(ctx, endUser, businessLine)
}

func (x routedGrants) ToReader(ctx context.Context, endUser, to string) ([]*memory.Grant, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Grants.ToReader(ctx, endUser, to)
}

func (x routedGrants) DeleteBusinessLine(ctx context.Context, endUser, businessLine string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Grants.DeleteBusinessLine(ctx, endUser, businessLine)
}

func (x routedGrants) Create(ctx context.Context, g *memory.Grant) error {
	s, err := x.r.pick(ctx, shard.EndUser, g.EndUser)
	if err != nil {
		return err
	}
	return s.Grants.Create(ctx, g)
}

type routedSnapshots struct{ r *storeRouter }

var _ session.Snapshots = routedSnapshots{}

func (x routedSnapshots) Get(ctx context.Context, sessionID string) (*v1.SessionSnapshot, error) {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return nil, err
	}
	return s.Snapshots.Get(ctx, sessionID)
}

func (x routedSnapshots) Delete(ctx context.Context, sessionID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Snapshots.Delete(ctx, sessionID)
}

func (x routedSnapshots) Put(ctx context.Context, snap *v1.SessionSnapshot) error {
	s, err := x.r.pick(ctx, shard.SessionData, snap.GetSessionId())
	if err != nil {
		return err
	}
	return s.Snapshots.Put(ctx, snap)
}

type routedUsage struct{ r *storeRouter }

var _ usage.Store = routedUsage{}

func (x routedUsage) Record(ctx context.Context, e *usage.Entry) error {
	s, err := x.r.pick(ctx, shard.EndUser, e.EndUser)
	if err != nil {
		return err
	}
	return s.Usage.Record(ctx, e)
}

func (x routedUsage) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Usage.DeleteEndUser(ctx, businessLine, endUser)
}

func (x routedUsage) AnonymizeEntry(ctx context.Context, endUser, id string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Usage.AnonymizeEntry(ctx, endUser, id)
}

type routedTiers struct{ r *storeRouter }

var _ usage.TierStore = routedTiers{}

func (x routedTiers) Get(ctx context.Context, businessLine, endUser string) (string, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return "", err
	}
	return s.Tiers.Get(ctx, businessLine, endUser)
}

func (x routedTiers) Put(ctx context.Context, businessLine, endUser, tier string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Tiers.Put(ctx, businessLine, endUser, tier, at)
}

func (x routedTiers) Delete(ctx context.Context, businessLine, endUser string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Tiers.Delete(ctx, businessLine, endUser)
}

type routedPresence struct{ r *storeRouter }

var _ presence.Store = routedPresence{}

func (x routedPresence) Touch(ctx context.Context, sessionID, connID string, expires time.Time) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Presence.Touch(ctx, sessionID, connID, expires)
}

func (x routedPresence) Remove(ctx context.Context, sessionID, connID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Presence.Remove(ctx, sessionID, connID)
}

func (x routedPresence) List(ctx context.Context, sessionID string, now time.Time) ([]*presence.Entry, uint64, error) {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return nil, 0, err
	}
	return s.Presence.List(ctx, sessionID, now)
}

func (x routedPresence) Wait(ctx context.Context, sessionID string, version uint64) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Presence.Wait(ctx, sessionID, version)
}

func (x routedPresence) DeleteSession(ctx context.Context, sessionID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Presence.DeleteSession(ctx, sessionID)
}

func (x routedPresence) Put(ctx context.Context, e *presence.Entry) error {
	s, err := x.r.pick(ctx, shard.SessionData, e.SessionID)
	if err != nil {
		return err
	}
	return s.Presence.Put(ctx, e)
}

type routedPush struct{ r *storeRouter }

var _ notify.Registry = routedPush{}

func (x routedPush) Touch(ctx context.Context, businessLine, endUser, deviceID string, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Push.Touch(ctx, businessLine, endUser, deviceID, at)
}

func (x routedPush) List(ctx context.Context, businessLine, endUser string) ([]*notify.Device, error) {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return nil, err
	}
	return s.Push.List(ctx, businessLine, endUser)
}

func (x routedPush) Unregister(ctx context.Context, businessLine, endUser, deviceID string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Push.Unregister(ctx, businessLine, endUser, deviceID)
}

func (x routedPush) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	s, err := x.r.pick(ctx, shard.EndUser, endUser)
	if err != nil {
		return err
	}
	return s.Push.DeleteEndUser(ctx, businessLine, endUser)
}

func (x routedPush) Register(ctx context.Context, d *notify.Device, at time.Time) error {
	s, err := x.r.pick(ctx, shard.EndUser, d.EndUser)
	if err != nil {
		return err
	}
	return s.Push.Register(ctx, d, at)
}

type routedOutbox struct{ r *storeRouter }

var _ outbox.Store = routedOutbox{}

func (x routedOutbox) DeleteSession(ctx context.Context, sessionID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Outbox.DeleteSession(ctx, sessionID)
}
