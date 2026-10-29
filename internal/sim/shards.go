package sim

import (
	"context"
	"errors"
	"fmt"
	"time"

	"yanshi/internal/app"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/outbox"
	"yanshi/internal/sandbox"
	"yanshi/internal/shard"
	"yanshi/internal/workqueue"
)

// checkDeletedShard 故意检查实际分片而非路由结果：错库残留同样违反删除不变量。
func (w *World) checkDeletedShard(ctx context.Context, s app.Stores, sid string, keepMemory bool) error {
	for _, item := range []struct {
		q   workqueue.Queue
		key string
	}{{s.Queue, sid}, {s.SandboxQueue, sandbox.NodeID(sid)}} {
		has, err := item.q.(workqueue.Inspector).Contains(ctx, item.key)
		if err != nil {
			return err
		}
		if has {
			return fmt.Errorf("invariant: deleted session %s retains queued work", sid)
		}
	}
	if events, err := eventlog.ReadAll(ctx, s.Log, sid, 0); err != nil {
		return err
	} else if len(events) > 0 {
		return fmt.Errorf("invariant: deleted session %s retains events", sid)
	}
	if snap, err := s.Snapshots.Get(ctx, sid); err != nil {
		return err
	} else if snap != nil {
		return fmt.Errorf("invariant: deleted session %s retains snapshot", sid)
	}
	meta := s.ArtifactMeta
	if local, ok := meta.(pgartifact.Meta); ok {
		local.Select = nil
		meta = local
	}
	if rows, err := meta.List(ctx, sid); err != nil {
		return err
	} else if len(rows) > 0 {
		return fmt.Errorf("invariant: deleted session %s retains artifacts", sid)
	}
	index := s.Index
	if local, ok := index.(pglifecycle.Index); ok {
		local.Select = nil
		index = local
	}
	if _, err := index.LookupSession(ctx, sid); !errors.Is(err, lifecycle.ErrNotFound) {
		return fmt.Errorf("invariant: deleted session %s retains index: %v", sid, err)
	}
	if rows, _, err := s.Presence.List(ctx, sid, time.Time{}); err != nil {
		return err
	} else if len(rows) > 0 {
		return fmt.Errorf("invariant: deleted session %s retains presence", sid)
	}
	if has, err := s.Ledger.HasSession(sid); err != nil {
		return err
	} else if has {
		return fmt.Errorf("invariant: deleted session %s retains ledger", sid)
	}
	activity := s.Activity.(interface {
		Contains(context.Context, string) (bool, error)
	})
	if has, err := activity.Contains(ctx, sandbox.NodeID(sid)); err != nil {
		return err
	} else if has {
		return fmt.Errorf("invariant: deleted session %s retains sandbox activity", sid)
	}
	memories := s.Memories.(interface {
		HasSourceSession(context.Context, string) (bool, error)
	})
	if has, err := memories.HasSourceSession(ctx, sid); err != nil {
		return err
	} else if has && !keepMemory {
		return fmt.Errorf("invariant: deleted session %s retains memories", sid)
	}
	rows, err := s.Outbox.List(ctx, "bl")
	if err != nil {
		return err
	}
	for _, e := range rows {
		if e.SessionID == sid && e.Kind != outbox.SessionDeleted {
			return fmt.Errorf("invariant: deleted session %s retains envelope", sid)
		}
	}
	boxes := []string{sandbox.NodeID(sid)}
	for _, n := range w.nodes {
		boxes = append(boxes, n.id)
	}
	for _, eu := range w.inboxUsers() {
		for _, box := range boxes {
			rows, _, err := s.Inbox.Pending(ctx, eu, box)
			if err != nil {
				return err
			}
			for _, inv := range rows {
				if inv.GetSessionId() == sid {
					return fmt.Errorf("invariant: deleted session %s retains inbox call", sid)
				}
			}
		}
	}
	return nil
}

func (w *World) countShardCoverage(locator shard.Locator) error {
	seen := map[int]bool{}
	userShards := map[string]map[int]bool{}
	all := append(append(append([]string{}, w.known()...), w.deleted...), w.closed...)
	for _, sid := range all {
		id, err := locator.Locate(shard.Target{Kind: shard.SessionData, Key: sid})
		if err != nil {
			return err
		}
		seen[id] = true
		events, err := w.log.Read(context.Background(), sid, 0, 1)
		if err != nil {
			return err
		}
		if len(events) > 0 && events[0].GetSessionCreated() != nil {
			eu := events[0].GetSessionCreated().GetEndUser()
			if userShards[eu] == nil {
				userShards[eu] = map[int]bool{}
			}
			userShards[eu][id] = true
			index, err := locator.Locate(shard.Target{Kind: shard.SessionIndex, Key: sid})
			if err != nil {
				return err
			}
			owner, err := locator.Locate(shard.Target{Kind: shard.EndUser, Key: eu})
			if err != nil {
				return err
			}
			if index != owner {
				return fmt.Errorf("invariant: Session index differs from EndUser shard")
			}
		}
	}
	w.Stats.ShardsWithSessions = len(seen)
	for _, locations := range userShards {
		if len(locations) > 1 {
			w.Stats.CrossShardUserSessions++
		}
	}
	for child, parent := range w.parentOf {
		c, err := locator.Locate(shard.Target{Kind: shard.SessionData, Key: child})
		if err != nil {
			return err
		}
		p, err := locator.Locate(shard.Target{Kind: shard.SessionData, Key: parent})
		if err != nil {
			return err
		}
		if c != p {
			w.Stats.CrossShardDelegations++
		}
	}
	return nil
}
