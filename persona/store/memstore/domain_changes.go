package memstore

import (
	"context"
	"slices"
	"strings"
	"time"

	"persona/domain"
	"persona/store"
)

func (s *Store) PendingDomainChanges(ctx context.Context, bl, eu string) ([]store.DomainChange, error) {
	out := []store.DomainChange{}
	err := s.View(ctx, bl, eu, func(b *domain.Book) error {
		for _, change := range b.PendingDomainChanges() {
			out = append(out, store.DomainChange{Owner: store.Owner{BusinessLine: bl, EndUser: eu}, DomainChange: change})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) AckDomainChanges(ctx context.Context, bl, eu, factID string, through uint64, now time.Time) error {
	return s.Update(ctx, bl, eu, func(b *domain.Book) error {
		return b.AckDomainChanges(factID, through, now)
	})
}

func (s *Store) PendingChangeOwners(ctx context.Context) ([]store.Owner, error) {
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
		pending := e.deletion.Status == "" && len(e.book.PendingDomainChanges()) > 0
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
