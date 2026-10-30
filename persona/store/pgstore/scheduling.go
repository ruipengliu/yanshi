package pgstore

import (
	"context"
	"slices"
	"strings"

	"persona/pg"
	"persona/store"
)

func (s *Store) SchedulingOwners(ctx context.Context) ([]store.Owner, error) {
	ctx, cancel := pg.Operation(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := pg.Acquire(ctx, s.pool)
	if err != nil {
		return nil, contextError(ctx, err)
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, `SELECT DISTINCT business_line,end_user FROM owners t WHERE NOT EXISTS (SELECT 1 FROM owner_deletions d WHERE d.business_line=t.business_line AND d.end_user=t.end_user)`)
	if err != nil {
		return nil, contextError(ctx, err)
	}
	defer rows.Close()
	out := []store.Owner{}
	for rows.Next() {
		var bl, eu []byte
		if err := rows.Scan(&bl, &eu); err != nil {
			return nil, err
		}
		out = append(out, store.Owner{BusinessLine: string(bl), EndUser: string(eu)})
	}
	if err := contextError(ctx, rows.Err()); err != nil {
		return nil, err
	}
	// Go 的字节顺序与内存实现一致，不依赖数据库的 locale。
	slices.SortFunc(out, func(a, b store.Owner) int {
		if n := strings.Compare(a.BusinessLine, b.BusinessLine); n != 0 {
			return n
		}
		return strings.Compare(a.EndUser, b.EndUser)
	})
	return out, nil
}
