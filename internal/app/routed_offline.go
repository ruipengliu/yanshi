package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/outbox"
	"yanshi/internal/shard"
	"yanshi/internal/usage"
)

func (x routedInbox) RemoveSession(ctx context.Context, endUser, sessionID string) error {
	if endUser != "" {
		s, err := x.r.pick(ctx, shard.EndUser, endUser)
		if err != nil {
			return err
		}
		return s.Inbox.RemoveSession(ctx, endUser, sessionID)
	}
	return x.r.each(ctx, func(s Stores) error { return s.Inbox.RemoveSession(ctx, "", sessionID) })
}
func (x routedLedger) Prune(before time.Time) error {
	return x.r.each(context.Background(), func(s Stores) error { return s.Ledger.Prune(before) })
}
func (x routedActivity) ClaimIdle(ctx context.Context, before time.Time, limit int) ([]string, error) {
	// ClaimIdle 会移除候选；拿到一批就交回，不能在后库失败时丢掉前库已认领的清理工作。
	x.r.scanMu.Lock()
	start := x.r.activityNext
	x.r.activityNext = (start + 1) % len(x.r.ids)
	x.r.scanMu.Unlock()
	for offset := range x.r.ids {
		id := x.r.ids[(start+offset)%len(x.r.ids)]
		s, err := x.r.at(ctx, id)
		if err != nil {
			return nil, err
		}
		ids, err := s.Activity.ClaimIdle(ctx, before, limit)
		if err != nil || len(ids) > 0 {
			return ids, err
		}
	}
	return nil, nil
}
func (x routedIndex) List(ctx context.Context, businessLine, endUser, cursor string, limit int) ([]*lifecycle.Session, string, error) {
	if endUser != "" {
		s, err := x.r.pick(ctx, shard.EndUser, endUser)
		if err != nil {
			return nil, "", err
		}
		return s.Index.List(ctx, businessLine, endUser, cursor, limit)
	}
	if limit <= 0 {
		return nil, "", fmt.Errorf("lifecycle: invalid list limit")
	}
	var rows []*lifecycle.Session
	// 全局 (LastInputAt, ID) 游标也是每库的安全续扫位置；取每库前 limit+1 再合并，无需丢弃未消费的行。
	err := x.r.each(ctx, func(s Stores) error {
		page, _, err := s.Index.List(ctx, businessLine, "", cursor, limit+1)
		rows = append(rows, page...)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].LastInputAt.Equal(rows[j].LastInputAt) {
			return rows[i].LastInputAt.After(rows[j].LastInputAt)
		}
		return rows[i].ID > rows[j].ID
	})
	if len(rows) > limit {
		return rows[:limit], lifecycle.EncodeCursor(rows[limit-1]), nil
	}
	return rows, "", nil
}
func (x routedIndex) scan(ctx context.Context, limit int, fn func(lifecycle.Index) ([]string, error)) ([]string, error) {
	var rows []string
	err := x.r.each(ctx, func(s Stores) error {
		part, err := fn(s.Index)
		rows = append(rows, part...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return sortedLimit(rows, limit), nil
}
func (x routedIndex) ScheduledAfter(ctx context.Context, businessLine, afterID string, limit int) ([]string, error) {
	return x.scan(ctx, limit, func(s lifecycle.Index) ([]string, error) { return s.ScheduledAfter(ctx, businessLine, afterID, limit) })
}
func (x routedIndex) IdleBefore(ctx context.Context, businessLine string, agents lifecycle.AgentFilter, before time.Time, limit int) ([]string, error) {
	return x.scan(ctx, limit, func(s lifecycle.Index) ([]string, error) {
		return s.IdleBefore(ctx, businessLine, agents, before, limit)
	})
}
func (x routedIndex) ClosedBefore(ctx context.Context, businessLine string, agents lifecycle.AgentFilter, before time.Time, limit int) ([]string, error) {
	return x.scan(ctx, limit, func(s lifecycle.Index) ([]string, error) {
		return s.ClosedBefore(ctx, businessLine, agents, before, limit)
	})
}
func (x routedIndex) DueBefore(ctx context.Context, businessLine string, before time.Time, limit int) ([]string, error) {
	return x.scan(ctx, limit, func(s lifecycle.Index) ([]string, error) { return s.DueBefore(ctx, businessLine, before, limit) })
}
func (x routedDeletions) Pending(ctx context.Context, limit int) ([]string, error) {
	var rows []string
	err := x.r.each(ctx, func(s Stores) error {
		part, err := s.Deletions.Pending(ctx, limit)
		rows = append(rows, part...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return sortedLimit(rows, limit), nil
}
func (x routedDeletions) PendingEndUsers(ctx context.Context, limit int) ([]*lifecycle.EndUserTombstone, error) {
	var rows []*lifecycle.EndUserTombstone
	err := x.r.each(ctx, func(s Stores) error {
		part, err := s.Deletions.PendingEndUsers(ctx, limit)
		rows = append(rows, part...)
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].BusinessLine != rows[j].BusinessLine {
			return rows[i].BusinessLine < rows[j].BusinessLine
		}
		return rows[i].EndUser < rows[j].EndUser
	})
	if limit >= 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}
func (x routedDeletions) PruneEndUsers(ctx context.Context, now time.Time, limit int, cleanup func(context.Context, string, string) error) error {
	return x.r.each(ctx, func(s Stores) error { return s.Deletions.PruneEndUsers(ctx, now, limit, cleanup) })
}
func (x routedDeletions) WaitDeleted(ctx context.Context, sessionID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Deletions.(lifecycle.Watcher).WaitDeleted(ctx, sessionID)
}
func (x routedDeletions) CreateRequest(ctx context.Context, req *lifecycle.Request) error {
	s, err := x.r.at(ctx, 0)
	if err != nil {
		return err
	}
	return s.Deletions.CreateRequest(ctx, req)
}
func (x routedDeletions) RequestInfo(ctx context.Context, id string) (*lifecycle.Request, error) {
	s, err := x.r.at(ctx, 0)
	if err != nil {
		return nil, err
	}
	return s.Deletions.(lifecycle.RequestProgress).RequestInfo(ctx, id)
}
func (x routedDeletions) CountRequest(ctx context.Context, id string) (int, int, error) {
	var total, completed int
	err := x.r.each(ctx, func(s Stores) error {
		n, done, err := s.Deletions.(lifecycle.RequestProgress).CountRequest(ctx, id)
		total += n
		completed += done
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return total, completed, nil
}
func (x routedDeletions) Request(ctx context.Context, id string) (*lifecycle.Request, error) {
	r, err := x.RequestInfo(ctx, id)
	if err != nil {
		return nil, err
	}
	r.Sessions, r.Completed, err = x.CountRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (x routedMemories) DeleteSession(ctx context.Context, sessionID string) error {
	return x.r.each(ctx, func(s Stores) error { return s.Memories.DeleteSession(ctx, sessionID) })
}
func (x routedMemories) RecordAccess(ctx context.Context, accesses []memory.Access) error {
	// 先校验所有键，再按真实 EndUser 拆批；同库不同用户也不能混在一个事务里。
	groups := map[string][]memory.Access{}
	var users []string
	for _, a := range accesses {
		if _, err := x.r.pick(ctx, shard.EndUser, a.EndUser); err != nil {
			return err
		}
		if _, ok := groups[a.EndUser]; !ok {
			users = append(users, a.EndUser)
		}
		groups[a.EndUser] = append(groups[a.EndUser], a)
	}
	sort.Strings(users)
	for _, eu := range users {
		s, err := x.r.pick(ctx, shard.EndUser, eu)
		if err != nil {
			return err
		}
		if err := s.Memories.RecordAccess(ctx, groups[eu]); err != nil {
			return err
		}
	}
	return nil
}
func (x routedUsage) Spent(ctx context.Context, businessLine, endUser string, from, to time.Time) (int64, error) {
	if endUser != "" && endUser != usage.Anonymous {
		s, err := x.r.pick(ctx, shard.EndUser, endUser)
		if err != nil {
			return 0, err
		}
		return s.Usage.Spent(ctx, businessLine, endUser, from, to)
	}
	var total int64
	err := x.r.each(ctx, func(s Stores) error {
		part, err := s.Usage.Spent(ctx, businessLine, endUser, from, to)
		total += part
		return err
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
func (x routedUsage) Report(ctx context.Context, query usage.Query) ([]usage.Row, error) {
	if query.EndUser != "" && query.EndUser != usage.Anonymous {
		s, err := x.r.pick(ctx, shard.EndUser, query.EndUser)
		if err != nil {
			return nil, err
		}
		return s.Usage.Report(ctx, query)
	}
	rows := map[string]usage.Row{}
	err := x.r.each(ctx, func(s Stores) error {
		part, err := s.Usage.Report(ctx, query)
		if err != nil {
			return err
		}
		for _, p := range part {
			r := rows[p.Key]
			r.Key = p.Key
			r.Calls += p.Calls
			r.Cost += p.Cost
			r.InputTokens += p.InputTokens
			r.CachedInputTokens += p.CachedInputTokens
			r.OutputTokens += p.OutputTokens
			r.SandboxMillis += p.SandboxMillis
			rows[p.Key] = r
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []usage.Row
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (x routedUsage) Prune(ctx context.Context, before time.Time) error {
	return x.r.each(ctx, func(s Stores) error { return s.Usage.Prune(ctx, before) })
}
func (x routedPresence) Prune(ctx context.Context, before time.Time) error {
	return x.r.each(ctx, func(s Stores) error { return s.Presence.Prune(ctx, before) })
}
func (x routedOutbox) List(ctx context.Context, businessLine string) ([]outbox.Envelope, error) {
	var rows []outbox.Envelope
	err := x.r.each(ctx, func(s Stores) error {
		part, err := s.Outbox.List(ctx, businessLine)
		rows = append(rows, part...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
func (x routedOutbox) Maintain(ctx context.Context, clk clock.Clock, batch int) error {
	return x.r.each(ctx, func(s Stores) error { return s.Outbox.Maintain(ctx, clk, batch) })
}
func (x routedOutbox) Stats(ctx context.Context) (outbox.Stats, error) {
	var total outbox.Stats
	err := x.r.each(ctx, func(s Stores) error {
		part, err := s.Outbox.Stats(ctx)
		total.Rows += part.Rows
		total.Bytes += part.Bytes
		total.WatermarkLag = max(total.WatermarkLag, part.WatermarkLag)
		total.OldestTransaction = max(total.OldestTransaction, part.OldestTransaction)
		return err
	})
	if err != nil {
		return outbox.Stats{}, err
	}
	return total, nil
}

// C5a 不把库 0 的消费结果冒充整个单元；多分片游标与订阅留 C5b。
func (x routedOutbox) Read(ctx context.Context, req outbox.Request) (outbox.Page, error) {
	if len(x.r.ids) != 1 || req.Shard != 0 {
		return outbox.Page{}, outbox.ErrShard
	}
	return x.r.stores[0].Outbox.Read(ctx, req)
}
func (x routedOutbox) Head(ctx context.Context, req outbox.Request) (string, error) {
	if len(x.r.ids) != 1 || req.Shard != 0 {
		return "", outbox.ErrShard
	}
	return x.r.stores[0].Outbox.Head(ctx, req)
}
func (x routedOutbox) Subscribe(businessLine string) (<-chan struct{}, func()) {
	if len(x.r.ids) != 1 {
		return nil, func() {}
	}
	return x.r.stores[0].Outbox.Subscribe(businessLine)
}
