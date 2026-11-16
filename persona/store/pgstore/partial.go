package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"persona/domain"
	"persona/pg"
	"persona/store"
)

// 按需读取（Persona ADR-0002 第一期）：只读事务先装载小表，再由领域的读取计划按条件读物化行。
// partialSkipped 是留给计划装载的大表，与 domain.Book.MarkPartial 清空的字段一致。
var partialSkipped = []string{"Changesets", "Exchanges", "Commitments", "Days", "Weeks", "Merges", "DomainChanges"}

// loadPlanned 在 plan 能限定范围且物化行是当前叠加版本时按需装载，否则整体装载。
func loadPlanned(ctx context.Context, tx pgx.Tx, bl, eu []byte, plan domain.ReadPlan) (*domain.Book, error) {
	if plan != nil {
		book, err := loadPartialBook(ctx, tx, bl, eu)
		if err != nil {
			return nil, err
		}
		if book != nil {
			ok, err := plan(book, pgRows{ctx: ctx, tx: tx, bl: bl, eu: eu})
			if err != nil {
				return nil, err
			}
			if ok {
				return book, nil
			}
		}
	}
	book, _, err := loadBook(ctx, tx, bl, eu)
	return book, err
}

// loadPartialBook 只装载小表；owner 不存在或物化行不是当前叠加版本时返回 nil，由调用方整体装载。
func loadPartialBook(ctx context.Context, tx pgx.Tx, bl, eu []byte) (*domain.Book, error) {
	snapshot := bookSnapshot{}
	for _, c := range categories {
		if slices.Contains(partialSkipped, c.field) {
			continue
		}
		rows, err := loadRows(ctx, tx, bl, eu, c)
		if err != nil {
			return nil, fmt.Errorf("load persona %s: %w", c.table, err)
		}
		if c.field == "Meta" {
			if len(rows) != 1 {
				return nil, nil
			}
			var meta storedMeta
			if err := json.Unmarshal(rows[0].payload, &meta); err != nil {
				return nil, err
			}
			if meta.FoldVersion != domain.FoldVersion {
				return nil, nil
			}
		}
		snapshot[c.field] = rows
	}
	book, err := bookFromRows(snapshot)
	if err != nil {
		return nil, err
	}
	book.MarkPartial()
	return book, nil
}

// pgRows 在同一只读快照中按条件读物化行。SQL 只做粗选（可以多选），精确条件由领域的 Match 判断；
// 含 U+0000 的行以 JSON 字符串保存，SQL 无法取字段，一律粗选进来。
type pgRows struct {
	ctx    context.Context
	tx     pgx.Tx
	bl, eu []byte
}

type rowFilter struct {
	conds []string
	args  []any
}

func (f *rowFilter) any(field string, values []string) {
	if len(values) > 0 {
		f.args = append(f.args, values)
		f.conds = append(f.conds, fmt.Sprintf("payload->>'%s' = ANY($%d)", field, len(f.args)+2))
	}
}

func loadEntities[T any](r pgRows, c category, f rowFilter, match func(*T) bool) ([]T, error) {
	if len(f.conds) == 0 {
		return nil, nil
	}
	conds := append(f.conds, "jsonb_typeof(payload) = 'string'")
	query := "SELECT payload FROM " + c.table + " WHERE business_line=$1 AND end_user=$2 AND (" + strings.Join(conds, " OR ") +
		") ORDER BY " + c.table + ".ordinal," + c.table + "." + c.key
	rows, err := r.tx.Query(r.ctx, query, append([]any{r.bl, r.eu}, f.args...)...)
	if err != nil {
		return nil, fmt.Errorf("load persona %s: %w", c.table, err)
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		store.RecordLoad(r.ctx, len(payload), 0)
		var value T
		if err := unmarshalPayload(payload, &value); err != nil {
			return nil, err
		}
		if match(&value) {
			out = append(out, value)
		}
	}
	return out, rows.Err()
}

func categoryOf(field string) category {
	i := slices.IndexFunc(categories, func(c category) bool { return c.field == field })
	return categories[i]
}

func (r pgRows) Commitments(f domain.CommitmentRows) ([]domain.Commitment, error) {
	var q rowFilter
	q.any("id", f.IDs)
	q.any("session", f.Sessions)
	if f.Unmerged {
		cond := "COALESCE(payload->>'merged_into', '') = ''"
		if len(f.Statuses) > 0 {
			q.args = append(q.args, f.Statuses)
			cond += fmt.Sprintf(" AND payload->>'status' = ANY($%d)", len(q.args)+2)
		}
		q.conds = append(q.conds, "("+cond+")")
	}
	return loadEntities(r, categoryOf("Commitments"), q, f.Match)
}

func (r pgRows) Exchanges(f domain.ExchangeRows) ([]domain.Exchange, error) {
	var q rowFilter
	q.any("session", f.Sessions)
	q.any("date", f.Dates)
	return loadEntities(r, categoryOf("Exchanges"), q, f.Match)
}

func (r pgRows) Changesets(f domain.ChangesetRows) ([]domain.Changeset, error) {
	var q rowFilter
	q.any("id", f.IDs)
	q.any("session", f.Sessions)
	return loadEntities(r, categoryOf("Changesets"), q, f.Match)
}

func (r pgRows) Days(f domain.DayRows) ([]domain.Day, error) {
	var q rowFilter
	q.any("date", f.Dates)
	return loadEntities(r, categoryOf("Days"), q, f.Match)
}

// StaleOwners 按 book_meta 中的叠加版本发现需要重建的 owner，不读取账本载荷。
func (s *Store) StaleOwners(ctx context.Context) ([]store.Owner, error) {
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
	rows, err := conn.Query(ctx, `SELECT m.business_line, m.end_user FROM book_meta m
WHERE COALESCE((m.payload->>'FoldVersion')::int, 0) <> $1
AND NOT EXISTS (SELECT 1 FROM owner_deletions d WHERE d.business_line=m.business_line AND d.end_user=m.end_user)`, domain.FoldVersion)
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
	slices.SortFunc(out, func(a, b store.Owner) int {
		if n := strings.Compare(a.BusinessLine, b.BusinessLine); n != 0 {
			return n
		}
		return strings.Compare(a.EndUser, b.EndUser)
	})
	return out, nil
}
