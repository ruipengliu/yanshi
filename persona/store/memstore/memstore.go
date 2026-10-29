// Package memstore 是 Persona 的内存存储，开发与评测和后续持久存储共用同一个领域核心。
package memstore

import (
	"context"
	"slices"
	"strings"
	"sync"

	"persona/domain"
	"persona/store"
)

type key struct{ businessLine, endUser string }
type entry struct {
	mu       sync.Mutex
	book     *domain.Book
	captures captureData
}
type Store struct {
	mu    sync.Mutex
	books map[key]*entry
}

var _ store.Store = (*Store)(nil)

func New() *Store { return &Store{books: map[key]*entry{}} }
func (s *Store) entry(bl, eu string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.books == nil {
		s.books = map[key]*entry{}
	}
	k := key{bl, eu}
	e := s.books[k]
	if e == nil {
		e = &entry{book: domain.NewBook()}
		s.books[k] = e
	}
	return e
}

func (s *Store) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	working, err := e.book.Clone()
	if err != nil {
		return err
	}
	captures := newCaptureTx(ctx, e.captures)
	defer captures.close()
	working.SetCaptureAccess(captures)
	if err := fn(working); err != nil {
		return err
	}
	// 提交时再复制，防止回调把可变引用带出事务。
	committed, err := working.Clone()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.book, e.captures = committed, captures.commit()
	return nil
}

func (s *Store) View(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	snapshot, err := e.book.Clone()
	captures := newCaptureTx(ctx, e.captures)
	e.mu.Unlock()
	defer captures.close()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot.SetCaptureAccess(captures)
	if err := fn(snapshot); err != nil {
		return err
	}
	return ctx.Err()
}

// PendingDeletions 先复制键再逐键查看，避免全局锁与 Book 锁嵌套。
// 发现结果不是认领；编排器须在执行时重读待办并保持幂等。
func (s *Store) PendingDeletions(ctx context.Context) ([]store.Owner, error) {
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
		pending := len(e.book.PendingDeletionTasks()) > 0
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

// 保留同键 entry，防止已经取得 entry 的调用与重置后的调用使用两把锁。
func (s *Store) ResetOwner(ctx context.Context, bl, eu string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	e.book, e.captures = domain.NewBook(), nil
	return nil
}
