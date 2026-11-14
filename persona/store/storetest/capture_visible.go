package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

func captureVisible(t *testing.T, factory func() store.Store) {
	t.Run("pages_progress_queries_erase", func(t *testing.T) {
		s := factory()
		ctx := context.Background()
		batches := make([][]domain.CaptureSegment, 3)
		for batch := range batches {
			batches[batch] = make([]domain.CaptureSegment, 130)
			for i := range batches[batch] {
				batches[batch][i] = domain.CaptureSegment{Speaker: "unknown", Text: "隐藏", Background: true}
			}
		}
		old := captureNow.Add(-time.Hour)
		batches[0][1] = domain.CaptureSegment{Speaker: "unknown", Text: "可见甲", SpokenAt: old.Format(time.RFC3339), TimeZone: "UTC"}
		batches[2][128] = domain.CaptureSegment{Speaker: "wearer", SpeakerGrade: "timeline", Text: "可见乙", SpokenAt: old.Add(time.Minute).Format(time.RFC3339), TimeZone: "UTC"}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if err := b.BindDay(captureDate, "day"); err != nil {
				return err
			}
			for i, batch := range batches {
				if _, err := b.AppendCaptures(captureDate, fmt.Sprint(i), batch); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// 新事务从真实存储读取，隐藏区跨批次与 PG 的 128 条预读页。
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			p, err := domain.NewCall(b, "day", []byte(`{"limit":1}`), nil, captureNow).Captures(captureOptions())
			if err != nil {
				return err
			}
			if p.Kind != "page" || p.Done || p.Ended || len(p.Segments) != 1 || p.Segments[0].SpeakerKey != "u-1-1" {
				t.Fatal("first visible page", p)
			}
			c := domain.NewCall(b, "day", []byte(`{"source":{"from":"#1.1","to":"#3.128"}}`), nil, captureNow)
			c.FileChanges()
			if c.Err() != nil {
				return c.Err()
			}
			if b.CaptureProgress(captureDate) != "0" {
				t.Fatal("unread tail advanced")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			p, err := domain.NewCall(b, "day", []byte(`{"after":"1.1~1.1","limit":1}`), nil, captureNow).Captures(captureOptions())
			if err != nil {
				return err
			}
			if !p.Cont || !p.Done || p.Span != "1.1" || len(p.Segments) != 1 || p.Segments[0].SpeakerKey != "u-3-128" {
				t.Fatal("visible continuation", p)
			}
			for _, source := range []string{`{"from":"#1.0","to":"#3.128"}`, `{"from":"#1.1","to":"#3.129"}`} {
				c := domain.NewCall(b, "day", json.RawMessage(`{"source":`+source+`}`), nil, captureNow)
				if text := c.FileChanges(); !strings.Contains(text, "没有被接受") || c.Err() != nil {
					t.Fatal("hidden source endpoint accepted", text, c.Err())
				}
			}
			c := domain.NewCall(b, "day", []byte(`{"source":{"from":"#1.1","to":"#3.128"}}`), nil, captureNow)
			if text := c.FileChanges(); strings.Contains(text, "没有被接受") || c.Err() != nil {
				t.Fatal(text, c.Err())
			}
			if b.CaptureProgress(captureDate) != "3.128" || !b.SchedulingDays()[0].Processed {
				t.Fatal("hidden tail prevented completion")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			a := b.CaptureAccess()
			g, found, err := a.Lookup(captureDate, domain.Ref{Seq: 1})
			if err != nil || !found || !g.Background {
				t.Fatal("physical lookup filtered hidden segment", err)
			}
			r, found, err := a.Adjacent(captureDate, domain.Ref{}, true)
			if err != nil || !found || r.Ref.String() != "3.129" {
				t.Fatal("physical adjacent filtered hidden segment", err)
			}
			count := 0
			for _, err := range a.Scan(captureDate, domain.Ref{}, domain.Ref{}) {
				if err != nil {
					return err
				}
				count++
			}
			if count != 390 {
				t.Fatal("physical scan filtered hidden segments", count)
			}
			batch, err := a.Batch(captureDate, 3)
			if err != nil || len(batch) != 130 || !batch[128].Equal(batches[2][128]) || !batch[129].Equal(batches[2][129]) {
				t.Fatal("batch identity changed", err)
			}
			views, more, err := b.QueryCaptures(captureDate, "3.127", 1)
			if err != nil || !more || len(views) != 1 || views[0].Speaker != "wearer" || views[0].SpeakerGrade != "timeline" {
				t.Fatal("app query projected identity", views, more, err)
			}
			all, err := b.DayCaptures(captureDate)
			if err != nil || len(all) != 390 || !all[0].Background {
				t.Fatal("app view hid segments", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if _, err := b.AppendCaptures(captureDate, "2", batches[2]); err != nil {
				return err
			}
			_, err := b.Erase(captureDate, "3.128", "3.128", captureNow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			if b.CaptureProgress(captureDate) != "0" || b.SchedulingDays()[0].Processed {
				t.Fatal("erasure failed to rewind visible span")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("all_hidden_and_read_error", func(t *testing.T) {
		s := factory()
		ctx := context.Background()
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if err := b.BindDay(captureDate, "day"); err != nil {
				return err
			}
			_, err := b.AppendCaptures(captureDate, "hidden", []domain.CaptureSegment{{Speaker: "unknown", Text: "隐藏", Disposition: "quarantine", DispositionReason: "playback"}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		failure := errors.New("hidden scan failed")
		err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			b.SetCaptureAccess(&captureReadFault{CaptureAccess: b.CaptureAccess(), operation: "scan", failure: failure})
			_, err := domain.NewCall(b, "day", nil, nil, captureNow).Captures(captureOptions())
			return err
		})
		if !errors.Is(err, failure) {
			t.Fatal("hidden scan swallowed error", err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			p, err := domain.NewCall(b, "day", nil, nil, captureNow).Captures(captureOptions())
			if err != nil {
				return err
			}
			if p.Kind != "empty" || b.CaptureProgress(captureDate) != "0" || !b.SchedulingDays()[0].Processed {
				t.Fatal("all hidden day has pending captures", p)
			}
			_, err = b.Erase(captureDate, "1.0", "1.0", captureNow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
