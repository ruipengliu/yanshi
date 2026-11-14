package memstore

import (
	"context"
	"slices"
	"strings"
	"time"

	"persona/domain"
	"persona/store"
)

func (s *Store) BeginOwnerDeletion(ctx context.Context, bl, eu string, now time.Time) (store.OwnerDeletion, error) {
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	if now.IsZero() {
		return store.OwnerDeletion{}, store.ErrDeletionTime
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	if e.deletion.Status == "" {
		e.deletion = store.OwnerDeletion{Owner: store.Owner{BusinessLine: bl, EndUser: eu}, Status: "pending", StartedAt: now.UTC()}
	}
	return e.deletion, nil
}

func (s *Store) OwnerDeletion(ctx context.Context, bl, eu string) (store.OwnerDeletion, error) {
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	if e.deletion.Status == "" {
		return store.OwnerDeletion{}, store.ErrDeletionNotStarted
	}
	return e.deletion, nil
}

func (s *Store) PendingPurges(ctx context.Context) ([]store.Owner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	entries := make(map[key]*entry, len(s.books))
	for k, e := range s.books {
		entries[k] = e
	}
	s.mu.Unlock()
	out := []store.Owner{}
	for k, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e.mu.Lock()
		pending := e.deletion.Status == "pending"
		e.mu.Unlock()
		if pending {
			out = append(out, store.Owner{BusinessLine: k.businessLine, EndUser: k.endUser})
		}
	}
	slices.SortFunc(out, func(a, b store.Owner) int {
		if n := strings.Compare(a.BusinessLine, b.BusinessLine); n != 0 {
			return n
		}
		return strings.Compare(a.EndUser, b.EndUser)
	})
	return out, ctx.Err()
}

func (s *Store) Purge(ctx context.Context, bl, eu string, limit int, now time.Time) (store.OwnerDeletion, error) {
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	if limit < 1 || limit > store.MaxPurgeBatch {
		return store.OwnerDeletion{}, store.ErrPurgeLimit
	}
	if now.IsZero() {
		return store.OwnerDeletion{}, store.ErrDeletionTime
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.OwnerDeletion{}, err
	}
	if e.deletion.Status == "" {
		return store.OwnerDeletion{}, store.ErrDeletionNotStarted
	}
	if e.deletion.Status == "completed" {
		return e.deletion, nil
	}
	left := limit
	for key, segments := range e.captures {
		if left == 0 {
			break
		}
		n := min(left, len(segments))
		// View 持有独立的批次目录；片段数组仍不可变，不能 clear 旧快照的正文。
		if n == len(segments) {
			delete(e.captures, key)
		} else {
			e.captures[key] = domain.CloneCaptureSegments(segments[n:])
		}
		left -= n
	}
	for key := range e.pending {
		if left == 0 {
			break
		}
		delete(e.pending, key)
		left--
	}
	if left > 0 {
		left -= e.book.PurgeBatch(left)
	}
	if left > 0 {
		e.captures = nil
		e.deletion.Status, e.deletion.CompletedAt = "completed", now.UTC()
		if e.deletion.CompletedAt.Before(e.deletion.StartedAt) {
			e.deletion.CompletedAt = e.deletion.StartedAt
		}
	}
	return e.deletion, nil
}
