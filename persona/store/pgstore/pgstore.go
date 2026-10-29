// Package pgstore 在 Persona 的独立数据库中保存账本与原文。
package pgstore

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"persona/domain"
	"persona/pg"
	"persona/store"
)

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ store.Store = (*Store)(nil)

// Open 使用 Persona 专用 DSN；迁移由 pg.Migrate 显式执行，不要求应用角色有 DDL 权限。
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pg.Open(ctx, dsn, "")
	if err != nil {
		return nil, err
	}
	return New(pool), nil
}

// New 接管专属连接池，调用方不得在 Close 后继续复用它。
func New(pool *pgxpool.Pool, now ...func() time.Time) *Store {
	s := &Store{pool: pool}
	if len(now) > 0 {
		s.now = now[0]
	}
	return s
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Update(ctx context.Context, businessLine, endUser string, fn func(*domain.Book) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return contextError(ctx, err)
	}
	defer pg.Rollback(tx)
	bl, eu := []byte(businessLine), []byte(endUser)
	// 首次写入也参与同键串行化：冲突插入会等前一个创建事务结束。
	tag, err := tx.Exec(ctx, `INSERT INTO owners (business_line,end_user) VALUES ($1,$2) ON CONFLICT DO NOTHING`, bl, eu)
	if err != nil {
		return contextError(ctx, err)
	}
	var locked []byte
	if err := tx.QueryRow(ctx, `SELECT business_line FROM owners WHERE business_line=$1 AND end_user=$2 FOR UPDATE`, bl, eu).Scan(&locked); err != nil {
		return contextError(ctx, err)
	}
	book := domain.NewBook()
	var before bookSnapshot
	if tag.RowsAffected() == 0 {
		book, before, err = loadBook(ctx, tx, bl, eu)
		if err != nil {
			return contextError(ctx, err)
		}
	}
	access := newCaptureTx(ctx, tx, bl, eu, false)
	defer access.close()
	book.SetCaptureAccess(access)
	book.SetCaptureClock(s.now)
	err = fn(book)
	// 在写回账本之前关闭接缝，逃逸的迭代器也不能在提交阶段继续访问。
	access.close()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	after, err := snapshotRows(book, before)
	if err != nil {
		return err
	}
	if err := saveBook(ctx, tx, bl, eu, before, after); err != nil {
		return contextError(ctx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contextError(ctx, err)
	}
	return nil
}

func (s *Store) View(ctx context.Context, businessLine, endUser string, fn func(*domain.Book) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return contextError(ctx, err)
	}
	defer pg.Rollback(tx)
	bl, eu := []byte(businessLine), []byte(endUser)
	book, _, err := loadBook(ctx, tx, bl, eu)
	if err != nil {
		return contextError(ctx, err)
	}
	access := newCaptureTx(ctx, tx, bl, eu, true)
	defer access.close()
	book.SetCaptureAccess(access)
	book.SetCaptureClock(s.now)
	if err := fn(book); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *Store) PendingDeletions(ctx context.Context) ([]store.Owner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT business_line,end_user FROM deletion_tasks WHERE status='pending' OR rebuild_pending`)
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

func (s *Store) ExpiringCaptures(ctx context.Context, now time.Time) ([]store.Owner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT business_line,end_user FROM business_days WHERE NOT capture_cleanup_done AND (expires_at <= $1 OR expires_at IS NULL)`, now)
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

func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// 外键级联覆盖原文和账本，避免只清 State 后遗留引用、墓碑或删除待办。
func (s *Store) ResetOwner(ctx context.Context, businessLine, endUser string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM owners WHERE business_line=$1 AND end_user=$2`, []byte(businessLine), []byte(endUser))
	return contextError(ctx, err)
}
