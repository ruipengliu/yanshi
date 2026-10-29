package domain_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"persona/domain"
	"persona/store/memstore"
)

func TestQueryWeekSurvivesRebinding(t *testing.T) {
	book := domain.NewBook()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	book.BindWeek("2026-10-26", "old")
	call := domain.NewCall(book, "old", json.RawMessage(`{"suggestions":["accepted"]}`), nil, now)
	_ = call.FileWeek()
	if call.Err() != nil {
		t.Fatal(call.Err())
	}
	book.BindWeek("2026-10-26", "new")
	week, err := book.QueryWeek("2026-10-26")
	if err != nil || week.Session != "old" || len(week.Suggestions) != 1 {
		t.Fatalf("lost accepted week: %v", err)
	}
}

func TestCaptureQueryDoesNotAdvanceModelProgress(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	books := memstore.New(func() time.Time { return now })
	ctx := context.Background()
	if err := books.Update(ctx, "bl", "u", func(book *domain.Book) error {
		_, err := book.AppendCaptures("2026-10-29", "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "one"}, {Speaker: "wearer", Text: "two"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := books.View(ctx, "bl", "u", func(book *domain.Book) error {
		before := book.CaptureProgress("2026-10-29")
		items, more, err := book.QueryCaptures("2026-10-29", "", 1)
		if err != nil || len(items) != 1 || !more {
			t.Fatalf("page: %v", err)
		}
		if book.CaptureProgress("2026-10-29") != before {
			t.Fatal("app query advanced model progress")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
