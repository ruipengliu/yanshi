// Package memstore 是 Persona 的内存存储，开发与评测和后续持久存储共用同一个领域核心。
package memstore

import (
	"context"
	"persona/domain"
	"persona/store"
	"sync"
)

type key struct{ businessLine, endUser string }
type entry struct {
	mu   sync.Mutex
	book *domain.Book
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
	e.book = committed
	return nil
}

func (s *Store) View(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	snapshot, err := e.book.Clone()
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(snapshot)
}
