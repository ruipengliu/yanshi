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
	created, err := lockOwner(ctx, tx, bl, eu)
	if err != nil {
		return contextError(ctx, err)
	}
	// 屏障与写入竞争同一行锁：先提交的写入归清理，后来的写入连回调也不进入。
	if err := checkOwner(ctx, tx, bl, eu); err != nil {
		return err
	}
	book := domain.NewBook()
	var before bookSnapshot
	if !created {
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
	if err := checkOwner(ctx, tx, bl, eu); err != nil {
		return err
	}
	book, _, err := loadBook(ctx, tx, bl, eu)
	if err != nil {
		return contextError(ctx, err)
	}
	access := newCaptureTx(ctx, tx, bl, eu, true)
	defer access.close()
	book.SetCaptureAccess(access)
	book.SetCaptureClock(s.now)
	callbackErr := fn(book)
	// 释放快照连接后再查最新屏障，单连接池也不会自锁；失败时入口必须丢弃回调结果。
	access.close()
	pg.Rollback(tx)
	if err := checkOwner(ctx, s.pool, bl, eu); err != nil {
		return err
	}
	return callbackErr
}

func (s *Store) PendingDeletions(ctx context.Context) ([]store.Owner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT business_line,end_user FROM deletion_tasks t WHERE (status='pending' OR rebuild_pending) AND NOT EXISTS (SELECT 1 FROM owner_deletions d WHERE d.business_line=t.business_line AND d.end_user=t.end_user)`)
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
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT business_line,end_user FROM business_days t WHERE NOT capture_cleanup_done AND (expires_at <= $1 OR expires_at IS NULL) AND NOT EXISTS (SELECT 1 FROM owner_deletions d WHERE d.business_line=t.business_line AND d.end_user=t.end_user)`, now)
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return contextError(ctx, err)
	}
	defer pg.Rollback(tx)
	bl, eu := []byte(businessLine), []byte(endUser)
	if _, err := lockOwner(ctx, tx, bl, eu); err != nil {
		return contextError(ctx, err)
	}
	if err := checkOwner(ctx, tx, bl, eu); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM owners WHERE business_line=$1 AND end_user=$2`, bl, eu); err != nil {
		return contextError(ctx, err)
	}
	return contextError(ctx, tx.Commit(ctx))
}
