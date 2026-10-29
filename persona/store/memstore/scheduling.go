package memstore

import (
	"context"
	"slices"
	"strings"

	"persona/store"
)

func (s *Store) SchedulingOwners(ctx context.Context) ([]store.Owner, error) {
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
		pending := e.deletion.Status == ""
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
