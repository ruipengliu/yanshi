// Package memstore 是 Persona 的内存存储，开发与评测和后续持久存储共用同一个领域核心。
package memstore

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"persona/domain"
	"persona/store"
)

type key struct{ businessLine, endUser string }
type entry struct {
	mu       sync.Mutex
	book     *domain.Book
	captures captureData
	pending  pendingData
	deletion store.OwnerDeletion
}
type Store struct {
	mu    sync.Mutex
	books map[key]*entry
	now   func() time.Time
	// PartialReads 让 ViewPlanned 也按读取计划只交出部分账本，供测试检查计划本身；默认整体装载，
	// 内存实现是按需读取的参照（Persona ADR-0002）。
	PartialReads bool
}

var _ store.Store = (*Store)(nil)

func New(now ...func() time.Time) *Store {
	s := &Store{books: map[key]*entry{}}
	if len(now) > 0 {
		s.now = now[0]
	}
	return s
}
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
	if e.deletion.Status != "" {
		return store.ErrOwnerDeleting
	}
	working, err := cloneForTransaction(ctx, e.book)
	if err != nil {
		return err
	}
	captures := newCaptureTx(ctx, e.captures, e.pending)
	defer captures.close()
	working.SetCaptureAccess(captures)
	working.SetCaptureClock(s.now)
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
	e.pending = captures.commitPending()
	return nil
}

func (s *Store) View(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := s.entry(bl, eu)
	e.mu.Lock()
	if e.deletion.Status != "" {
		e.mu.Unlock()
		return store.ErrOwnerDeleting
	}
	snapshot, err := cloneForTransaction(ctx, e.book)
	// 注销清理会逐项收回目录；只复制目录以保持在途 View 的片段快照不可变。
	captures := newCaptureTx(ctx, maps.Clone(e.captures), maps.Clone(e.pending))
	e.mu.Unlock()
	defer captures.close()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot.SetCaptureAccess(captures)
	snapshot.SetCaptureClock(s.now)
	callbackErr := fn(snapshot)
	captures.close()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deletion.Status != "" {
		return store.ErrOwnerDeleting
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return callbackErr
}

func (s *Store) ViewPlanned(ctx context.Context, bl, eu string, plan domain.ReadPlan, fn func(*domain.Book) error) error {
	if plan == nil || !s.PartialReads {
		return s.View(ctx, bl, eu, fn)
	}
	return s.View(ctx, bl, eu, func(snapshot *domain.Book) error {
		partial, rows := snapshot.PartialCopy()
		ok, err := plan(partial, rows)
		if err != nil {
			return err
		}
		if !ok {
			return fn(snapshot)
		}
		return fn(partial)
	})
}

// StaleOwners 总是为空：内存实现每次事务都由变更集整体叠加，没有物化行。
func (s *Store) StaleOwners(ctx context.Context) ([]store.Owner, error) {
	return nil, ctx.Err()
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
		pending := e.deletion.Status == "" && len(e.book.PendingDeletionTasks()) > 0
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
	if e.deletion.Status != "" {
		return store.ErrOwnerDeleting
	}
	e.book, e.captures = domain.NewBook(), nil
	e.pending = nil
	return nil
}

func (s *Store) ExpiringCaptures(ctx context.Context, now time.Time) ([]store.Owner, error) {
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
		pending := e.deletion.Status == "" && len(e.book.ExpiringCaptureDays(now)) > 0
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
