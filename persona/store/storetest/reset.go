package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"persona/domain"
	"persona/store"
)

// 重置必须清掉 State 看不见的原文、绑定、编号、墓碑与待办，重用 owner 才不会污染下一次试验。
func resetOwner(t *testing.T, factory func() store.Store) {
	s := factory()
	resetter, ok := s.(store.Resetter)
	if !ok {
		t.Skip("store does not support fixture reset")
	}
	ctx := context.Background()
	seedCaptures(t, s)
	seed := domain.SeedData{Commitments: []domain.SeedCommitment{{Kind: "mine", Content: "发报价"}}}
	populate := func(b *domain.Book) error {
		b.Seed(seed, captureNow)
		b.BindWeek("2026-10-21", "week")
		b.Submitted("session", "整理今天")
		call := domain.NewCall(b, "week", []byte(`{}`), nil, captureNow)
		call.FileWeek()
		return call.Err()
	}
	for _, eu := range []string{"u", "other"} {
		if err := s.Update(ctx, "bl", eu, populate); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(eu string) string {
		t.Helper()
		var data []byte
		if err := s.View(ctx, "bl", eu, func(b *domain.Book) error {
			var err error
			data, err = json.Marshal(b)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	other := snapshot("other")
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		_, err := b.Erase(captureDate, "1.0", "1.0", captureNow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshot("u")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := resetter.ResetOwner(cancelled, "bl", "u"); !errors.Is(err, context.Canceled) || snapshot("u") != before {
		t.Fatal("cancelled reset changed owner", err)
	}
	for range 2 {
		if err := resetter.ResetOwner(ctx, "bl", "u"); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := json.Marshal(domain.NewBook())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot("u") != string(empty) || snapshot("other") != other {
		t.Fatal("reset retained book facts or changed another owner")
	}
	if owners, err := s.PendingDeletions(ctx); err != nil || len(owners) != 0 {
		t.Fatal("reset retained pending owner", owners, err)
	}
	if changes, err := s.PendingDomainChanges(ctx, "bl", "u"); err != nil || len(changes) != 0 {
		t.Fatal("reset retained domain changes", changes, err)
	}
	// 同批次与同引用必须重新可用，且不能读到重置前的原文。
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(seed, captureNow)
		if b.State().Commitments[0].ID != "C-0001" {
			t.Fatal("reset retained ID sequence")
		}
		if err := b.BindDay(captureDate, "fresh"); err != nil {
			return err
		}
		refs, err := b.AppendCaptures(captureDate, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "新原文"}})
		if err != nil || len(refs) != 1 || refs[0] != "1.0" {
			t.Fatalf("reset append: %v %v", refs, err)
		}
		page, err := domain.NewCall(b, "fresh", nil, nil, captureNow).Captures(captureOptions())
		if err != nil || len(page.Segments) != 1 || page.Segments[0].Text != "新原文" {
			t.Fatal("reset retained captures", err)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
