package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

// 删除待办与原文共享提交点，适用于 memstore 及以后 P4 的持久实现。
func deletions(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	s := factory()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	const date = "2026-10-27"
	for _, owner := range []store.Owner{{BusinessLine: "a", EndUser: "u"}, {BusinessLine: "b", EndUser: "u"}} {
		if err := s.Update(ctx, owner.BusinessLine, owner.EndUser, func(b *domain.Book) error {
			if err := b.BindDay(date, "session"); err != nil {
				return err
			}
			_, err := b.AppendCaptures(date, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文"}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	fail := errors.New("rollback")
	if err := s.Update(ctx, "a", "u", func(b *domain.Book) error {
		if _, err := b.Erase(date, "1.0", "1.0", now); err != nil {
			return err
		}
		if len(b.PendingDeletionTasks()) != 1 {
			t.Fatal("task missing inside transaction")
		}
		return fail
	}); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if owners, err := s.PendingDeletions(ctx); err != nil || len(owners) != 0 {
		t.Fatal("rollback leaked pending owner", owners, err)
	}
	if err := s.View(ctx, "a", "u", func(b *domain.Book) error {
		p, err := domain.NewCall(b, "session", nil, nil, now).Captures(domain.PageOptions{Limit: 200, MaxChars: 6000, GapMS: 300000})
		if err != nil || len(p.Segments) != 1 || len(b.DeletionTasks()) != 0 {
			t.Fatal("rollback changed captures or tasks", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var escaped *domain.Book
	var task domain.DeletionTask
	if err := s.Update(ctx, "a", "u", func(b *domain.Book) error {
		var err error
		task, err = b.Erase(date, "1.0", "1.0", now)
		escaped = b
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := escaped.CompleteDeletionTask(task.ID, nil, now); err != nil {
		t.Fatal(err)
	}
	if owners, err := s.PendingDeletions(ctx); err != nil || !reflect.DeepEqual(owners, []store.Owner{{BusinessLine: "a", EndUser: "u"}}) {
		t.Fatal("pending owners", owners, err)
	}
	if err := s.View(ctx, "a", "u", func(b *domain.Book) error {
		if len(b.PendingDeletionTasks()) != 1 {
			t.Fatal("escaped reference completed task")
		}
		if err := b.BindDay(date, "replacement"); err != nil {
			return err
		}
		p, err := domain.NewCall(b, "replacement", nil, nil, now).Captures(domain.PageOptions{Limit: 200, MaxChars: 6000, GapMS: 300000})
		if err != nil || p.Kind != "empty" {
			t.Fatal("committed erasure retained text", err)
		}
		return b.CompleteDeletionTask(task.ID, nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	if owners, err := s.PendingDeletions(ctx); err != nil || len(owners) != 1 {
		t.Fatal("View completed task", owners, err)
	}
	if err := s.Update(ctx, "a", "u", func(b *domain.Book) error {
		return b.CompleteDeletionTask(task.ID, []domain.MemoryCandidate{{ID: "memory", Summary: "摘要"}}, now)
	}); err != nil {
		t.Fatal(err)
	}
	if owners, err := s.PendingDeletions(ctx); err != nil || len(owners) != 0 {
		t.Fatal("completed owner still pending", owners, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PendingDeletions(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
