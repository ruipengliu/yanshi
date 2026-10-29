package memstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"persona/domain"
)

func TestPurgeRemovesPhysicalData(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s := New(func() time.Time { return now })
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1"}}, Commitments: []domain.SeedCommitment{{Kind: "mine", Content: "待删"}}}, now)
		b.BindWeek("2026-10-21", "week")
		call := domain.NewCall(b, "week", []byte(`{}`), nil, now)
		call.FileWeek()
		if err := call.Err(); err != nil {
			return err
		}
		b.Submitted("day", "整理今天")
		_, err := b.AppendCaptures("2026-10-27", "batch", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e := s.entry("bl", "u")
	if _, err := s.BeginOwnerDeletion(ctx, "bl", "u", now); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		out, err := s.Purge(ctx, "bl", "u", 1, now)
		if err != nil {
			t.Fatal(err)
		}
		if out.Status == "completed" {
			break
		}
	}
	want, err := json.Marshal(domain.NewBook())
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(e.book)
	if err != nil || string(got) != string(want) || len(e.captures) != 0 || e.deletion.Status != "completed" {
		t.Fatal("physical data survived purge", err)
	}
	if s.entry("bl", "u") != e {
		t.Fatal("owner lock identity changed")
	}
}
