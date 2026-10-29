package app

import (
	"context"
	"errors"
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
		ctx, cancel := x.r.attempt(ctx)
		defer cancel()
		s, err := x.r.pick(ctx, shard.EndUser, endUser)
		if err != nil {
			return err
		}
		return s.Inbox.RemoveSession(ctx, endUser, sessionID)
	}
	return x.r.each(ctx, "Inbox.RemoveSession", func(ctx context.Context, s Stores) error { return s.Inbox.RemoveSession(ctx, "", sessionID) })
}
func (x routedLedger) Prune(before time.Time) error {
	return x.r.each(context.Background(), "Ledger.Prune", func(ctx context.Context, s Stores) error {
		if p, ok := s.Ledger.(interface {
			PruneContext(context.Context, time.Time) error
		}); ok {
			return p.PruneContext(ctx, before)
		}
		return s.Ledger.Prune(before)
	})
}
func (x routedActivity) ClaimIdle(ctx context.Context, before time.Time, limit int) ([]string, error) {
	if x.r.deterministic {
		x.r.scanMu.Lock()
		start := x.r.activityNext
		x.r.activityNext = (start + 1) % len(x.r.ids)
		x.r.scanMu.Unlock()
		for offset := range x.r.ids {
			s, err := x.r.at(ctx, x.r.ids[(start+offset)%len(x.r.ids)])
			if err != nil {
				return nil, err
			}
			rows, err := s.Activity.ClaimIdle(ctx, before, limit)
			if err != nil || len(rows) > 0 {
				return rows, err
			}
		}
		return nil, nil
	}
	// 每片取走的候选全部交回；其他片的失败不能让这批清理工作消失。
	parts := fanout(ctx, x.r, "Activity.ClaimIdle", func(ctx context.Context, s Stores, cursor string) ([]string, string, error) {
		rows, err := s.Activity.ClaimIdle(ctx, before, limit)
		return rows, cursor, err
	})
	var rows []string
	var errs []error
	for _, part := range parts {
		rows = append(rows, part.Value...)
		if part.Err != nil {
			errs = append(errs, part.Err)
		}
	}
	if len(rows) > 0 {
		return rows, independentError(errs)
	}
	return nil, errors.Join(errs...)
}

func (x routedIndex) List(ctx context.Context, businessLine, endUser, cursor string, limit int) ([]*lifecycle.Session, string, error) {
	if endUser != "" {
		ctx, cancel := x.r.attempt(ctx)
		defer cancel()
		s, err := x.r.pick(ctx, shard.EndUser, endUser)
		if err != nil {
			return nil, "", err
		}
		return s.Index.List(ctx, businessLine, endUser, cursor, limit)
	}
	if limit <= 0 {
		return nil, "", fmt.Errorf("lifecycle: invalid list limit")
	}
	parts, err := collect(ctx, x.r, "Index.List", true, func(ctx context.Context, s Stores) ([]*lifecycle.Session, error) {
		page, _, err := s.Index.List(ctx, businessLine, "", cursor, limit+1)
		return page, err
	})
	if err != nil {
		return nil, "", err
	}
	var rows []*lifecycle.Session
	for _, part := range parts {
		rows = append(rows, part...)
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
func (x routedIndex) scan(ctx context.Context, path string, limit int, fn func(context.Context, lifecycle.Index) ([]string, error)) ([]string, error) {
	return x.r.scanIDs(ctx, path, limit, func(ctx context.Context, s Stores) ([]string, error) { return fn(ctx, s.Index) })
}

func (x routedIndex) ScheduledAfter(ctx context.Context, businessLine, afterID string, limit int) ([]string, error) {
	if x.r.deterministic {
		return x.scan(ctx, "Index.ScheduledAfter", limit, func(ctx context.Context, s lifecycle.Index) ([]string, error) {
			return s.ScheduledAfter(ctx, businessLine, afterID, limit)
		})
	}
	return x.scan(ctx, "Index.ScheduledAfter:"+businessLine, limit, func(ctx context.Context, s lifecycle.Index) ([]string, error) {
		return s.ScheduledAfter(ctx, businessLine, lifecycle.ScanAfter(ctx), limit)
	})
}

func (x routedIndex) IdleBefore(ctx context.Context, businessLine string, agents lifecycle.AgentFilter, before time.Time, limit int) ([]string, error) {
	return x.scan(ctx, fmt.Sprintf("Index.IdleBefore:%q:%q:%q", businessLine, agents.Only, agents.Except), limit, func(ctx context.Context, s lifecycle.Index) ([]string, error) {
		return s.IdleBefore(ctx, businessLine, agents, before, limit)
	})
}
func (x routedIndex) ClosedBefore(ctx context.Context, businessLine string, agents lifecycle.AgentFilter, before time.Time, limit int) ([]string, error) {
	return x.scan(ctx, fmt.Sprintf("Index.ClosedBefore:%q:%q:%q", businessLine, agents.Only, agents.Except), limit, func(ctx context.Context, s lifecycle.Index) ([]string, error) {
		return s.ClosedBefore(ctx, businessLine, agents, before, limit)
	})
}
func (x routedIndex) DueBefore(ctx context.Context, businessLine string, before time.Time, limit int) ([]string, error) {
	return x.scan(ctx, "Index.DueBefore:"+businessLine, limit, func(ctx context.Context, s lifecycle.Index) ([]string, error) {
		return s.DueBefore(ctx, businessLine, before, limit)
	})
}
func (x routedDeletions) Pending(ctx context.Context, limit int) ([]string, error) {
	return x.r.scanIDs(ctx, "Deletions.Pending", limit, func(ctx context.Context, s Stores) ([]string, error) { return s.Deletions.Pending(ctx, limit) })
}
func (x routedDeletions) PendingEndUsers(ctx context.Context, limit int) ([]*lifecycle.EndUserTombstone, error) {
	parts := fanout(ctx, x.r, "Deletions.PendingEndUsers", func(ctx context.Context, s Stores, cursor string) ([]*lifecycle.EndUserTombstone, string, error) {
		if x.r.deterministic {
			cursor = ""
		}
		rows, err := s.Deletions.PendingEndUsers(lifecycle.WithScanAfter(ctx, cursor), limit)
		next := ""
		if limit > 0 && len(rows) == limit {
			last := rows[len(rows)-1]
			next = last.BusinessLine + "\x00" + last.EndUser
		}
		return rows, next, err
	})
	var rows []*lifecycle.EndUserTombstone
	var errs []error
	success := false
	for _, part := range parts {
		if part.Err != nil {
			errs = append(errs, part.Err)
		} else {
			rows = append(rows, part.Value...)
			success = true
		}
	}
	if !success || (x.r.deterministic && len(errs) > 0) {
		return nil, errors.Join(errs...)
	}
	if !x.r.deterministic {
		return rows, independentError(errs)
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
	return x.r.each(ctx, "Deletions.PruneEndUsers", func(ctx context.Context, s Stores) error { return s.Deletions.PruneEndUsers(ctx, now, limit, cleanup) })
}
func (x routedDeletions) WaitDeleted(ctx context.Context, sessionID string) error {
	s, err := x.r.pick(ctx, shard.SessionData, sessionID)
	if err != nil {
		return err
	}
	return s.Deletions.(lifecycle.Watcher).WaitDeleted(ctx, sessionID)
}
func (x routedDeletions) CreateRequest(ctx context.Context, req *lifecycle.Request) error {
	ctx, cancel := x.r.attempt(ctx)
	defer cancel()
	s, err := x.r.at(ctx, 0)
	if err != nil {
		return err
	}
	return s.Deletions.CreateRequest(ctx, req)
}
func (x routedDeletions) RequestInfo(ctx context.Context, id string) (*lifecycle.Request, error) {
	ctx, cancel := x.r.attempt(ctx)
	defer cancel()
	s, err := x.r.at(ctx, 0)
	if err != nil {
		return nil, err
	}
	return s.Deletions.(lifecycle.RequestProgress).RequestInfo(ctx, id)
}
func (x routedDeletions) CountRequest(ctx context.Context, id string) (int, int, error) {
	parts, err := collect(ctx, x.r, "Deletions.CountRequest", true, func(ctx context.Context, s Stores) ([2]int, error) {
		n, done, err := s.Deletions.(lifecycle.RequestProgress).CountRequest(ctx, id)
		return [2]int{n, done}, err
	})
	if err != nil {
		return 0, 0, err
	}
	var total, completed int
	for _, part := range parts {
		total += part[0]
		completed += part[1]
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
	return x.r.each(ctx, "Memories.DeleteSession", func(ctx context.Context, s Stores) error { return s.Memories.DeleteSession(ctx, sessionID) })
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
		ctx, cancel := x.r.attempt(ctx)
		defer cancel()
		s, err := x.r.pick(ctx, shard.EndUser, endUser)
		if err != nil {
			return 0, err
		}
		return s.Usage.Spent(ctx, businessLine, endUser, from, to)
	}
	return x.r.spent(ctx, businessLine, endUser, from, to)
}

func (x routedUsage) Report(ctx context.Context, query usage.Query) ([]usage.Row, error) {
	if query.EndUser != "" && query.EndUser != usage.Anonymous {
		ctx, cancel := x.r.attempt(ctx)
		defer cancel()
		s, err := x.r.pick(ctx, shard.EndUser, query.EndUser)
		if err != nil {
			return nil, err
		}
		return s.Usage.Report(ctx, query)
	}
	parts, err := collect(ctx, x.r, "Usage.Report", true, func(ctx context.Context, s Stores) ([]usage.Row, error) { return s.Usage.Report(ctx, query) })
	if err != nil {
		return nil, err
	}
	rows := map[string]usage.Row{}
	for _, part := range parts {
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
	}
	var out []usage.Row
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (x routedUsage) Prune(ctx context.Context, before time.Time) error {
	return x.r.each(ctx, "Usage.Prune", func(ctx context.Context, s Stores) error { return s.Usage.Prune(ctx, before) })
}
func (x routedPresence) Prune(ctx context.Context, before time.Time) error {
	return x.r.each(ctx, "Presence.Prune", func(ctx context.Context, s Stores) error { return s.Presence.Prune(ctx, before) })
}
func (x routedOutbox) List(ctx context.Context, businessLine string) ([]outbox.Envelope, error) {
	parts, err := collect(ctx, x.r, "Outbox.List", true, func(ctx context.Context, s Stores) ([]outbox.Envelope, error) {
		return s.Outbox.List(ctx, businessLine)
	})
	if err != nil {
		return nil, err
	}
	var rows []outbox.Envelope
	for _, part := range parts {
		rows = append(rows, part...)
	}
	return rows, nil
}
func (x routedOutbox) Maintain(ctx context.Context, clk clock.Clock, batch int) error {
	return x.r.each(ctx, "Outbox.Maintain", func(ctx context.Context, s Stores) error { return s.Outbox.Maintain(ctx, clk, batch) })
}
func (x routedOutbox) Stats(ctx context.Context) (outbox.Stats, error) {
	parts, err := collect(ctx, x.r, "Outbox.Stats", true, func(ctx context.Context, s Stores) (outbox.Stats, error) { return s.Outbox.Stats(ctx) })
	if err != nil {
		return outbox.Stats{}, err
	}
	var total outbox.Stats
	for _, part := range parts {
		total.Rows += part.Rows
		total.Bytes += part.Bytes
		total.WatermarkLag = max(total.WatermarkLag, part.WatermarkLag)
		total.OldestTransaction = max(total.OldestTransaction, part.OldestTransaction)
	}
	return total, nil
}

func (x routedOutbox) Read(ctx context.Context, req outbox.Request) (outbox.Page, error) {
	if _, ok := x.r.stores[req.Shard]; !ok {
		return outbox.Page{}, outbox.ErrShard
	}
	ctx, cancel := x.r.attempt(ctx)
	defer cancel()
	s, err := x.r.at(ctx, req.Shard)
	if err != nil {
		return outbox.Page{}, err
	}
	return s.Outbox.Read(ctx, req)
}
func (x routedOutbox) Head(ctx context.Context, req outbox.Request) (string, error) {
	if _, ok := x.r.stores[req.Shard]; !ok {
		return "", outbox.ErrShard
	}
	ctx, cancel := x.r.attempt(ctx)
	defer cancel()
	s, err := x.r.at(ctx, req.Shard)
	if err != nil {
		return "", err
	}
	return s.Outbox.Head(ctx, req)
}
func (x routedOutbox) Subscribe(businessLine string) (<-chan struct{}, func()) {
	return x.SubscribeShard(0, businessLine)
}
func (x routedOutbox) SubscribeShard(id int, businessLine string) (<-chan struct{}, func()) {
	s, ok := x.r.stores[id]
	if !ok {
		return nil, func() {}
	}
	return s.Outbox.Subscribe(businessLine)
}
func (x routedOutbox) ShardIDs() []int            { return append([]int(nil), x.r.ids...) }
func (x routedOutbox) Identity() (string, string) { return x.r.locator.Identity() }

func (x routedOutbox) Slots(id int) []int { return x.r.locator.Slots(id) }

// 游标环扫保证暂时不能完成的候选会重试，也不会永久挡住同库后面的候选。
func (r *storeRouter) scanIDs(ctx context.Context, path string, limit int, fn func(context.Context, Stores) ([]string, error)) ([]string, error) {
	parts := fanout(ctx, r, path, func(ctx context.Context, s Stores, cursor string) ([]string, string, error) {
		if r.deterministic {
			cursor = ""
		}
		rows, err := fn(lifecycle.WithScanAfter(ctx, cursor), s)
		next := ""
		if limit > 0 && len(rows) == limit {
			next = rows[len(rows)-1]
		}
		return rows, next, err
	})
	var rows []string
	var errs []error
	success := false
	for _, part := range parts {
		if part.Err != nil {
			errs = append(errs, part.Err)
		} else {
			rows = append(rows, part.Value...)
			success = true
		}
	}
	if !success || (r.deterministic && len(errs) > 0) {
		return nil, errors.Join(errs...)
	}
	if r.deterministic {
		return sortedLimit(rows, limit), nil
	}
	// 每库 limit，不能截掉已推进游标的候选；总批量至多分片数乘 limit。
	return rows, independentError(errs)
}
