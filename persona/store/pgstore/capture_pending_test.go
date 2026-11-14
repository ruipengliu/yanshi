package pgstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"persona/domain"
	"persona/pg/pgtest"
)

func TestPendingPhysicalLifecycle(t *testing.T) {
	for _, operation := range []string{"erase", "day", "expire", "purge", "reset"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			captureNow := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			pool := pgtest.New(t)
			s := New(pool, func() time.Time { return captureNow })
			count := func() int {
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM capture_pending`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			stage := func(b *domain.Book) error {
				for _, seq := range []uint64{1, 3} {
					id := "first"
					if seq == 3 {
						id = "pending"
					}
					_, err := b.ReceiveCaptures("phone", domain.OrderedCapture{
						SessionID: "s", Seq: seq, CaptureID: id, BusinessDay: "2026-10-27", TimeZone: "UTC",
						Segments: []domain.CaptureSegment{{Speaker: "wearer", SpeakerGrade: "timeline", Disposition: "quarantine", DispositionReason: "playback", Text: "待清除\x00正文", SpokenAt: "2026-10-27T12:00:00Z", LowSupportSpans: []domain.LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"unknown"}}}}},
					})
					if err != nil {
						return err
					}
				}
				return nil
			}
			if err := s.Update(ctx, "bl", "u", stage); err != nil {
				t.Fatal(err)
			}
			if count() != 1 {
				t.Fatal("pending missing")
			}
			erase := func(b *domain.Book) error {
				_, err := b.Erase("2026-10-27", "1.0", "1.0", captureNow)
				return err
			}
			if err := s.View(ctx, "bl", "u", erase); err != nil {
				t.Fatal(err)
			}
			if count() != 1 {
				t.Fatal("View deleted pending")
			}
			rollback := errors.New("rollback")
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				if err := erase(b); err != nil {
					return err
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if count() != 1 {
				t.Fatal("rollback deleted pending")
			}
			switch operation {
			case "purge":
				if _, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow); err != nil {
					t.Fatal(err)
				}
				completed := false
				for range 100 {
					out, err := s.Purge(ctx, "bl", "u", 1, captureNow)
					if err != nil {
						t.Fatal(err)
					}
					if out.Status == "completed" {
						completed = true
						break
					}
				}
				if !completed {
					t.Fatal("purge did not finish")
				}
			case "reset":
				if err := s.ResetOwner(ctx, "bl", "u"); err != nil {
					t.Fatal(err)
				}
			default:
				if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
					switch operation {
					case "erase":
						return erase(b)
					case "day":
						_, err := b.EraseDay("2026-10-27", captureNow)
						return err
					default:
						at, _ := domain.CaptureExpiresAt("2026-10-27", "UTC")
						return b.ExpireCaptures("2026-10-27", at)
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			if count() != 0 {
				t.Fatal("pending survived lifecycle cleanup")
			}
			var remaining int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM capture_segments WHERE payload IS NOT NULL`).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatal("segment metadata survived lifecycle cleanup")
			}
		})
	}
}
