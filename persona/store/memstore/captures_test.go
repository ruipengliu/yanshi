package memstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"persona/domain"
	"persona/store/memstore"
)

// 原文、绑定、处理进度与变更集共享同一个提交点，失败事务不能只留下进度。
func TestCaptureProgressTransaction(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	opts := domain.PageOptions{Limit: 200, MaxChars: 6000, GapMS: 300000, MaxToolResult: 8000}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if err := b.BindDay("2026-10-27", "s"); err != nil {
			return err
		}
		if _, err := b.AppendCaptures("2026-10-27", "a", []domain.CaptureSegment{{Speaker: "wearer", Text: "交付", SpokenAt: now.Add(-time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}); err != nil {
			return err
		}
		_, err := domain.NewCall(b, "s", nil, nil, now).Captures(opts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("transaction failed")
	accept := func(b *domain.Book) error {
		c := domain.NewCall(b, "s", json.RawMessage(`{"source":{"from":"#1.0","to":"#1.0"}}`), nil, now)
		c.FileChanges()
		if c.Ack() == nil || b.CaptureProgress("2026-10-27") != "1.0" {
			t.Fatal("range not accepted")
		}
		return nil
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if err := accept(b); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		if b.CaptureProgress("2026-10-27") != "0" || len(b.State().Changesets) != 0 {
			t.Fatal("partial commit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "bl", "u", accept); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		if b.CaptureProgress("2026-10-27") != "1.0" || len(b.State().Changesets) != 1 {
			t.Fatal("missing commit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range [][2]string{{"other", "u"}, {"bl", "other"}} {
		if err := s.View(ctx, key[0], key[1], func(b *domain.Book) error {
			if _, err := domain.NewCall(b, "s", nil, nil, now).Captures(opts); err == nil {
				t.Fatal("cross-user capture access")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
