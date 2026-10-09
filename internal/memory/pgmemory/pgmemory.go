// Package pgmemory 是 memory.Store 与 memory.Grants 的 PostgreSQL 实现（向量检索使用 pgvector）。
package pgmemory

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/memory"
)

type Store struct{ Pool *pgxpool.Pool }

var _ memory.Store = Store{}

// vectorText 把向量编码为 pgvector 的文本形式，避免引入额外的驱动依赖。
func vectorText(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func (s Store) Put(ctx context.Context, m *memory.Memory, vec *memory.Vector) error {
	var emb, model *string
	if vec != nil {
		t := vectorText(vec.Values)
		emb, model = &t, &vec.Model
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO memories (id, business_line, end_user, category, content, source_session, source_call, created_at, updated_at, embedding, embedding_model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::public.vector, $11)
		ON CONFLICT (id) DO UPDATE SET business_line = $2, end_user = $3, category = $4, content = $5, source_session = $6,
			source_call = $7, created_at = $8, updated_at = $9, embedding = $10::text::public.vector, embedding_model = $11`,
		m.ID, m.BusinessLine, m.EndUser, string(m.Category), m.Content, m.SourceSession, m.SourceCall, m.CreatedAt, m.UpdatedAt, emb, model)
	return err
}

const cols = `id, business_line, end_user, category, content, source_session, source_call, created_at, updated_at`

func scan(row pgx.CollectableRow) (*memory.Memory, error) {
	m := &memory.Memory{}
	var cat string
	if err := row.Scan(&m.ID, &m.BusinessLine, &m.EndUser, &cat, &m.Content, &m.SourceSession, &m.SourceCall, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.Category, m.CreatedAt, m.UpdatedAt = memory.Category(cat), m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

func (s Store) Get(ctx context.Context, id string) (*memory.Memory, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+cols+` FROM memories WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, memory.ErrNotFound
	}
	return m, err
}

func (s Store) Delete(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM memories WHERE id = $1`, id)
	return err
}

func (s Store) Count(ctx context.Context, businessLine, endUser string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE business_line = $1 AND end_user = $2`, businessLine, endUser).Scan(&n)
	return n, err
}

// scopeFilter 把可读范围编码为两个并列数组参数：业务线与逗号分隔的类别（空串表示全部类别）。
func scopeFilter(scopes []memory.Scope) (lines, cats []string) {
	for _, sc := range scopes {
		var cs []string
		for _, c := range sc.Categories {
			cs = append(cs, string(c))
		}
		lines = append(lines, sc.BusinessLine)
		cats = append(cats, strings.Join(cs, ","))
	}
	return lines, cats
}

const inScope = `end_user = $1 AND EXISTS (
	SELECT 1 FROM unnest($2::text[], $3::text[]) AS sc(line, cats)
	WHERE sc.line = memories.business_line AND (sc.cats = '' OR memories.category = ANY (string_to_array(sc.cats, ','))))`

func (s Store) List(ctx context.Context, endUser string, scopes []memory.Scope, limit int) ([]*memory.Memory, error) {
	lines, cats := scopeFilter(scopes)
	rows, err := s.Pool.Query(ctx, `SELECT `+cols+` FROM memories WHERE `+inScope+`
		ORDER BY updated_at DESC, id LIMIT $4`, endUser, lines, cats, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

func (s Store) Nearest(ctx context.Context, endUser string, scopes []memory.Scope, vec memory.Vector, limit int) ([]memory.Hit, error) {
	lines, cats := scopeFilter(scopes)
	rows, err := s.Pool.Query(ctx, `
		SELECT `+cols+`, 1 - (embedding OPERATOR(public.<=>) $5::text::public.vector) AS score FROM memories
		WHERE `+inScope+` AND embedding_model = $6 AND vector_dims(embedding) = $7
		ORDER BY score DESC, updated_at DESC, id LIMIT $4`,
		endUser, lines, cats, limit, vectorText(vec.Values), vec.Model, len(vec.Values))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (memory.Hit, error) {
		m := &memory.Memory{}
		var cat string
		var score float64
		err := row.Scan(&m.ID, &m.BusinessLine, &m.EndUser, &cat, &m.Content, &m.SourceSession, &m.SourceCall, &m.CreatedAt, &m.UpdatedAt, &score)
		m.Category, m.CreatedAt, m.UpdatedAt = memory.Category(cat), m.CreatedAt.UTC(), m.UpdatedAt.UTC()
		return memory.Hit{Memory: m, Score: score}, err
	})
}

func (s Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM memories WHERE source_session = $1`, sessionID)
	return err
}

func (s Store) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM memories WHERE business_line = $1 AND end_user = $2`, businessLine, endUser)
	return err
}

func (s Store) RecordAccess(ctx context.Context, accesses []memory.Access) error {
	batch := &pgx.Batch{}
	for _, a := range accesses {
		batch.Queue(`INSERT INTO memory_access (memory_id, owner_line, end_user, reader_line, session_id, run_id, at)
			SELECT $1, $2, $3, $4, $5, $6, $7 WHERE EXISTS (SELECT 1 FROM memories WHERE id = $1)`,
			a.MemoryID, a.Owner, a.EndUser, a.Reader, a.SessionID, a.RunID, a.At)
	}
	return s.Pool.SendBatch(ctx, batch).Close()
}

func (s Store) Accesses(ctx context.Context, endUser, owner string, since time.Time, limit int) ([]memory.Access, error) {
	rows, err := s.Pool.Query(ctx, `SELECT memory_id, owner_line, end_user, reader_line, session_id, run_id, at FROM memory_access
		WHERE end_user = $1 AND owner_line = $2 AND at >= $3 ORDER BY at DESC, memory_id LIMIT $4`, endUser, owner, since, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (memory.Access, error) {
		var a memory.Access
		err := row.Scan(&a.MemoryID, &a.Owner, &a.EndUser, &a.Reader, &a.SessionID, &a.RunID, &a.At)
		a.At = a.At.UTC()
		return a, err
	})
}

type Grants struct{ Pool *pgxpool.Pool }

var _ memory.Grants = Grants{}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (g Grants) Create(ctx context.Context, gr *memory.Grant) error {
	cats := make([]string, len(gr.Categories))
	for i, c := range gr.Categories {
		cats[i] = string(c)
	}
	_, err := g.Pool.Exec(ctx, `INSERT INTO memory_grants (id, end_user, from_line, to_line, categories, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO NOTHING`,
		gr.ID, gr.EndUser, gr.From, gr.To, cats, gr.CreatedAt, nullTime(gr.ExpiresAt))
	return err
}

const grantCols = `id, end_user, from_line, to_line, categories, created_at, expires_at, revoked_at`

func scanGrant(row pgx.CollectableRow) (*memory.Grant, error) {
	gr := &memory.Grant{}
	var cats []string
	var exp, rev *time.Time
	if err := row.Scan(&gr.ID, &gr.EndUser, &gr.From, &gr.To, &cats, &gr.CreatedAt, &exp, &rev); err != nil {
		return nil, err
	}
	for _, c := range cats {
		gr.Categories = append(gr.Categories, memory.Category(c))
	}
	gr.CreatedAt = gr.CreatedAt.UTC()
	if exp != nil {
		gr.ExpiresAt = exp.UTC()
	}
	if rev != nil {
		gr.RevokedAt = rev.UTC()
	}
	return gr, nil
}

func (g Grants) Get(ctx context.Context, id string) (*memory.Grant, error) {
	rows, err := g.Pool.Query(ctx, `SELECT `+grantCols+` FROM memory_grants WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	gr, err := pgx.CollectExactlyOneRow(rows, scanGrant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, memory.ErrNotFound
	}
	return gr, err
}

func (g Grants) Revoke(ctx context.Context, id string, at time.Time) error {
	_, err := g.Pool.Exec(ctx, `UPDATE memory_grants SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, at)
	return err
}

func (g Grants) query(ctx context.Context, where string, args ...any) ([]*memory.Grant, error) {
	rows, err := g.Pool.Query(ctx, `SELECT `+grantCols+` FROM memory_grants WHERE `+where+` ORDER BY created_at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanGrant)
}

func (g Grants) List(ctx context.Context, endUser, businessLine string) ([]*memory.Grant, error) {
	return g.query(ctx, `end_user = $1 AND (from_line = $2 OR to_line = $2)`, endUser, businessLine)
}

func (g Grants) ToReader(ctx context.Context, endUser, to string) ([]*memory.Grant, error) {
	return g.query(ctx, `end_user = $1 AND to_line = $2`, endUser, to)
}

func (g Grants) DeleteBusinessLine(ctx context.Context, endUser, businessLine string) error {
	_, err := g.Pool.Exec(ctx, `DELETE FROM memory_grants WHERE end_user = $1 AND (from_line = $2 OR to_line = $2)`, endUser, businessLine)
	return err
}
