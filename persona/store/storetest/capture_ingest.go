package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

func orderedBatch(seq uint64) domain.OrderedCapture {
	return domain.OrderedCapture{SessionID: "recording", Seq: seq, CaptureID: fmt.Sprintf("c%d", seq), BusinessDay: captureDate, TimeZone: "UTC", Segments: []domain.CaptureSegment{{Speaker: "wearer", Text: fmt.Sprintf("有序正文%d", seq), SpokenAt: captureDate + "T12:00:00Z"}}}
}
func orderedCaptureIngest(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	t.Run("gap_duplicate_520_batches", func(t *testing.T) {
		s := factory()
		send := func(in domain.OrderedCapture) (domain.CaptureReceipt, error) {
			var out domain.CaptureReceipt
			err := s.Update(ctx, "bl", "u", func(b *domain.Book) error { var err error; out, err = b.ReceiveCaptures("phone", in); return err })
			return out, err
		}
		third := orderedBatch(3)
		third.Segments[0].SpokenAt = captureDate + "T10:00:00Z"
		third.Segments[0].Text = "有序正文3\x00尾部"
		for range 2 {
			out, err := send(third)
			if err != nil || out.Status != "staged" || out.NextSeq != 1 || len(out.Refs) != 0 {
				t.Fatal(out, err)
			}
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			captures, err := b.DayCaptures(captureDate)
			if err != nil {
				return err
			}
			if len(captures) != 0 || b.BusinessDay(captureDate).CaptureRevision != 0 {
				t.Fatal("staging became accepted")
			}
			raw, _ := json.Marshal(b)
			if strings.Contains(string(raw), "有序正文") {
				t.Fatal("raw staging text leaked to book")
			}
			if b.BusinessDay(captureDate).ExpiresAt.IsZero() {
				t.Fatal("staging has no expiry")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		bad := orderedBatch(3)
		if _, err := send(bad); !errors.Is(err, domain.ErrCaptureConflict) {
			t.Fatal(err)
		}
		if out, err := send(orderedBatch(1)); err != nil || !slices.Equal(out.Refs, []string{"1.0"}) {
			t.Fatal(out, err)
		}
		if out, err := send(orderedBatch(2)); err != nil || out.NextSeq != 4 || !slices.Equal(out.Refs, []string{"2.0"}) {
			t.Fatal(out, err)
		}
		if out, err := send(third); err != nil || !slices.Equal(out.Refs, []string{"3.0"}) {
			t.Fatal(out, err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			for seq := uint64(4); seq <= 520; seq++ {
				out, err := b.ReceiveCaptures("phone", orderedBatch(seq))
				if err != nil {
					return err
				}
				if !slices.Equal(out.Refs, []string{fmt.Sprintf("%d.0", seq)}) {
					return errors.New("reference reordered")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if out, err := send(orderedBatch(1)); err != nil || out.NextSeq != 521 || !slices.Equal(out.Refs, []string{"1.0"}) {
			t.Fatal(out, err)
		}
		bad = orderedBatch(1)
		bad.Segments[0].Text = "异文"
		if _, err := send(bad); !errors.Is(err, domain.ErrCaptureConflict) {
			t.Fatal(err)
		}
		bad = orderedBatch(521)
		bad.CaptureID = "c1"
		if _, err := send(bad); !errors.Is(err, domain.ErrCaptureConflict) {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if _, err := b.ReceiveCaptures("other-device", orderedBatch(521)); !errors.Is(err, domain.ErrCaptureConflict) {
				t.Fatal("device", err)
			}
			// 不能在第二条会话上合流；结束已补齐会话后才能开下一条。
			in := orderedBatch(1)
			in.SessionID = "second"
			in.CaptureID = "second"
			if _, err := b.ReceiveCaptures("phone", in); !errors.Is(err, domain.ErrCaptureConflict) {
				t.Fatal("concurrent stream", err)
			}
			if err := b.CloseCaptureSession("recording", "phone"); err != nil {
				return err
			}
			out, err := b.ReceiveCaptures("phone", in)
			if err == nil && !slices.Equal(out.Refs, []string{"521.0"}) {
				t.Fatal(out)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rollback_and_view", func(t *testing.T) {
		s := factory()
		fail := errors.New("rollback")
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error { _, err := b.ReceiveCaptures("phone", orderedBatch(2)); return err }); err != nil {
			t.Fatal(err)
		}
		accept := func(b *domain.Book) error {
			out, err := b.ReceiveCaptures("phone", orderedBatch(1))
			if err == nil && out.NextSeq != 3 {
				t.Fatal(out)
			}
			return err
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if err := accept(b); err != nil {
				return err
			}
			return fail
		}); !errors.Is(err, fail) {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", accept); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", accept); err != nil {
			t.Fatal(err)
		}
	})
	for _, operation := range []string{"erase", "day", "expire"} {
		t.Run(operation, func(t *testing.T) {
			s := factory()
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				for _, seq := range []uint64{1, 3} {
					if _, err := b.ReceiveCaptures("phone", orderedBatch(seq)); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				switch operation {
				case "erase":
					_, err := b.Erase(captureDate, "1.0", "1.0", captureNow)
					return err
				case "day":
					_, err := b.EraseDay(captureDate, captureNow)
					return err
				default:
					at, _ := domain.CaptureExpiresAt(captureDate, "UTC")
					return b.ExpireCaptures(captureDate, at)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				_, err := b.ReceiveCaptures("phone", orderedBatch(2))
				if !errors.Is(err, domain.ErrCaptureResync) {
					t.Fatal("old request accepted", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if operation == "erase" {
				if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
					in := orderedBatch(1)
					in.SessionID = "resynchronized"
					in.CaptureID = "new"
					in.Generation = 1
					out, err := b.ReceiveCaptures("phone", in)
					if err == nil && !slices.Equal(out.Refs, []string{"2.0"}) {
						t.Fatal("reference reused", out)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Run("midnight_and_invalid_pending", func(t *testing.T) {
		s := factory()
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			next := orderedBatch(2)
			next.BusinessDay = "2026-10-28"
			next.Segments[0].SpokenAt = "2026-10-28T00:01:00Z"
			if _, err := b.ReceiveCaptures("phone", next); err != nil {
				return err
			}
			invalid := orderedBatch(1)
			invalid.Segments[0].Text = ""
			if _, err := b.ReceiveCaptures("phone", invalid); !errors.Is(err, domain.ErrCaptureInvalid) {
				t.Fatal(err)
			}
			out, err := b.ReceiveCaptures("phone", orderedBatch(1))
			if err != nil {
				return err
			}
			if out.NextSeq != 3 {
				t.Fatal(out)
			}
			out, err = b.ReceiveCaptures("phone", next)
			if err == nil && (out.BusinessDay != "2026-10-28" || !slices.Equal(out.Refs, []string{"1.0"})) {
				t.Fatal(out)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("expiry_discovery_and_owner_barrier", func(t *testing.T) {
		s := factory()
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error { _, err := b.ReceiveCaptures("phone", orderedBatch(2)); return err }); err != nil {
			t.Fatal(err)
		}
		at, _ := domain.CaptureExpiresAt(captureDate, "UTC")
		owners, err := s.ExpiringCaptures(ctx, at)
		if err != nil || len(owners) != 1 {
			t.Fatal(owners, err)
		}
		if _, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error { t.Fatal("entered deleted owner"); return nil }); !errors.Is(err, store.ErrOwnerDeleting) {
			t.Fatal(err)
		}
		for range 100 {
			result, err := s.Purge(ctx, "bl", "u", 1, captureNow.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if result.Status == "completed" {
				return
			}
		}
		t.Fatal("purge did not finish")
	})
}

func concurrentCaptureIngest(t *testing.T, factory func() store.Store) {
	s := factory()
	ctx := context.Background()
	results := make(chan error, 2)
	for _, seq := range []uint64{1, 2} {
		go func() {
			results <- s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				_, err := b.ReceiveCaptures("phone", orderedBatch(seq))
				return err
			})
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		segments, err := b.DayCaptures(captureDate)
		if err != nil {
			return err
		}
		if len(segments) != 2 || segments[0].Ref != "1.0" || segments[1].Ref != "2.0" || segments[0].Text != "有序正文1" || segments[1].Text != "有序正文2" {
			return errors.New("concurrent upload reordered references")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
