package lifecycle

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"
)

func cloneEndUser(b *EndUserTombstone) *EndUserTombstone {
	cp := *b
	cp.Sessions = slices.Clone(b.Sessions)
	return &cp
}

func (d *MemDeletions) MarkEndUser(_ context.Context, b *EndUserTombstone) (*EndUserTombstone, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := [2]string{b.BusinessLine, b.EndUser}
	if d.endUsers == nil {
		d.endUsers = map[[2]string]*EndUserTombstone{}
	}
	if d.endUsers[k] == nil {
		d.endUsers[k] = &EndUserTombstone{BusinessLine: b.BusinessLine, EndUser: b.EndUser, RequestID: b.RequestID, RequestedAt: b.RequestedAt}
	}
	return cloneEndUser(d.endUsers[k]), nil
}

func (d *MemDeletions) GetEndUser(_ context.Context, businessLine, endUser string) (*EndUserTombstone, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.endUsers[[2]string{businessLine, endUser}]
	if b == nil {
		return nil, ErrNotFound
	}
	return cloneEndUser(b), nil
}

func (d *MemDeletions) AddEndUserSessions(_ context.Context, b *EndUserTombstone, ids []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if saved := d.endUsers[[2]string{b.BusinessLine, b.EndUser}]; saved != nil && saved.RequestID == b.RequestID {
		for _, id := range ids {
			if !slices.Contains(saved.Sessions, id) {
				saved.Sessions = append(saved.Sessions, id)
				saved.Revision++
				saved.CompletedAt, saved.ExpiresAt = time.Time{}, time.Time{}
			}
		}
		sort.Strings(saved.Sessions)
	}
	return nil
}

func (d *MemDeletions) SealEndUser(_ context.Context, b *EndUserTombstone) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if saved := d.endUsers[[2]string{b.BusinessLine, b.EndUser}]; saved != nil && saved.RequestID == b.RequestID {
		saved.Sealed = true
	}
	return nil
}

func (d *MemDeletions) PendingEndUsers(ctx context.Context, limit int) ([]*EndUserTombstone, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*EndUserTombstone
	for _, b := range d.endUsers {
		if b.Sealed && b.CompletedAt.IsZero() && b.BusinessLine+"\x00"+b.EndUser > ScanAfter(ctx) {
			out = append(out, cloneEndUser(b))
		}
	}
	slices.SortFunc(out, func(a, b *EndUserTombstone) int {
		if a.BusinessLine != b.BusinessLine {
			return strings.Compare(a.BusinessLine, b.BusinessLine)
		}
		return strings.Compare(a.EndUser, b.EndUser)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (d *MemDeletions) CompleteEndUser(_ context.Context, b *EndUserTombstone, at, expires time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if saved := d.endUsers[[2]string{b.BusinessLine, b.EndUser}]; saved != nil && saved.RequestID == b.RequestID && saved.Sealed && saved.Revision == b.Revision && saved.CompletedAt.IsZero() {
		saved.CompletedAt, saved.ExpiresAt = at, expires
	}
	return nil
}

func (d *MemDeletions) PruneEndUsers(ctx context.Context, now time.Time, limit int, cleanup func(context.Context, string, string) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var expired []*EndUserTombstone
	for _, b := range d.endUsers {
		if !b.CompletedAt.IsZero() && !b.ExpiresAt.After(now) {
			expired = append(expired, b)
		}
	}
	slices.SortFunc(expired, func(a, b *EndUserTombstone) int {
		if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
			return c
		}
		if a.BusinessLine != b.BusinessLine {
			return strings.Compare(a.BusinessLine, b.BusinessLine)
		}
		return strings.Compare(a.EndUser, b.EndUser)
	})
	var errs error
	for i, b := range expired {
		if i >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(errs, err)
		}
		// 与屏障移除串行，其他清理者不能拿着旧候选删除复用 ID 的数据。
		if cleanup != nil {
			if err := cleanup(ctx, b.BusinessLine, b.EndUser); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(errs, err)
		}
		delete(d.endUsers, [2]string{b.BusinessLine, b.EndUser})
	}
	return errs
}
