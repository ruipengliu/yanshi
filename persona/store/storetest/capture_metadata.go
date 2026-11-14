package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"persona/domain"
	"persona/store"
)

func metadataSegments() []domain.CaptureSegment {
	return []domain.CaptureSegment{
		{Speaker: "wearer", SpeakerGrade: "timeline", Disposition: "accept", Text: "甲🙂\x00乙", SpokenAt: captureDate + "T12:00:00Z", TimeZone: "UTC", LowSupportSpans: []domain.LowSupportSpan{{Start: 0, End: 2, CriticalKinds: []string{"person", "correction"}}, {Start: 3, End: 4}}},
		{Speaker: "unknown", Disposition: "quarantine", DispositionReason: "suspected_replay", Background: true, Text: "未知原文", SpokenAt: captureDate + "T12:01:00Z", TimeZone: "UTC", LowSupportSpans: []domain.LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"none"}}}},
		{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: "P-1", Background: true, Text: "背景原文", SpokenAt: captureDate + "T12:02:00Z", TimeZone: "UTC"},
	}
}

func captureMetadata(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	want := metadataSegments()
	mutate := func(g *domain.CaptureSegment) {
		g.LowSupportSpans[0].Start = 1
		g.LowSupportSpans[0].CriticalKinds[0] = "number"
	}
	check := func(a domain.CaptureAccess, seq uint64) error {
		batch, err := a.Batch(captureDate, seq)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(batch, want) {
			return fmt.Errorf("metadata batch differs")
		}
		mutate(&batch[0])
		g, found, err := a.Lookup(captureDate, domain.Ref{Seq: seq})
		if err != nil {
			return err
		}
		if !found || !reflect.DeepEqual(g, want[0]) {
			return fmt.Errorf("lookup aliased batch")
		}
		mutate(&g)
		for _, before := range []bool{false, true} {
			ref := domain.Ref{Seq: seq - 1, Index: 10}
			if before {
				ref = domain.Ref{Seq: seq, Index: 1}
			}
			r, found, err := a.Adjacent(captureDate, ref, before)
			if err != nil {
				return err
			}
			if !found || !reflect.DeepEqual(r.Segment, want[0]) {
				return fmt.Errorf("adjacent aliased lookup")
			}
			mutate(&r.Segment)
		}
		n := 0
		for r, err := range a.Scan(captureDate, domain.Ref{Seq: seq}, domain.Ref{Seq: seq, Index: 2}) {
			if err != nil {
				return err
			}
			if n >= len(want) || !reflect.DeepEqual(r.Segment, want[n]) {
				return fmt.Errorf("scan metadata differs")
			}
			if n == 0 {
				mutate(&r.Segment)
			}
			n++
		}
		if n != len(want) {
			return fmt.Errorf("scan count differs")
		}
		batch, err = a.Batch(captureDate, seq)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(batch, want) {
			return fmt.Errorf("read mutation reached storage")
		}
		return nil
	}
	t.Run("legacy_json_and_digest", func(t *testing.T) {
		s := factory()
		const legacy = `{"session_id":"s","seq":1,"capture_id":"c","business_day":"2026-10-27","time_zone":"UTC","generation":0,"segments":[{"speaker":"wearer","text":"原文","spoken_at":"2026-10-27T12:00:00Z","time_zone":"UTC"}]}`
		const digest = "ffb3a1d7d8eb603b709c63e0dc7d4b21986eb66b1a92d9990a53ec7716d5b07c"
		var in domain.OrderedCapture
		if err := json.Unmarshal([]byte(legacy), &in); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			_, err := b.ReceiveCaptures("phone", in)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			segments, err := b.CaptureAccess().Batch(captureDate, 1)
			if err != nil {
				return err
			}
			got := in
			got.Segments = segments
			raw, err := json.Marshal(got)
			if err != nil {
				return err
			}
			if string(raw) != legacy {
				return fmt.Errorf("legacy payload changed")
			}
			raw, err = json.Marshal(b)
			if err != nil {
				return err
			}
			if !strings.Contains(string(raw), digest) {
				return fmt.Errorf("legacy digest changed")
			}
			if _, err := b.ReceiveCaptures("phone", in); err != nil {
				return err
			}
			got.Segments[0].SpeakerGrade = "evidence"
			if _, err := b.ReceiveCaptures("phone", got); !errors.Is(err, domain.ErrCaptureConflict) {
				return fmt.Errorf("metadata absent from digest: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("copies_view_and_rollback", func(t *testing.T) {
		s := factory()
		input := domain.CloneCaptureSegments(want)
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if _, err := b.AppendCaptures(captureDate, "one", input); err != nil {
				return err
			}
			mutate(&input[0])
			return check(b.CaptureAccess(), 1)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			if err := check(b.CaptureAccess(), 1); err != nil {
				return err
			}
			if _, err := b.AppendCaptures(captureDate, "one", domain.CloneCaptureSegments(want)); err != nil {
				return err
			}
			changed := domain.CloneCaptureSegments(want)
			changed[0].LowSupportSpans[0].CriticalKinds[0] = "date"
			if _, err := b.AppendCaptures(captureDate, "one", changed); err == nil {
				return fmt.Errorf("metadata duplicate accepted")
			}
			input := domain.CloneCaptureSegments(want)
			if _, err := b.AppendCaptures(captureDate, "two", input); err != nil {
				return err
			}
			mutate(&input[0])
			if err := check(b.CaptureAccess(), 2); err != nil {
				return err
			}
			_, err := b.Erase(captureDate, "1.0", "1.1", captureNow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		failure := errors.New("rollback")
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if err := check(b.CaptureAccess(), 1); err != nil {
				return err
			}
			if _, err := b.AppendCaptures(captureDate, "two", want); err != nil {
				return err
			}
			if _, err := b.EraseDay(captureDate, captureNow); err != nil {
				return err
			}
			return failure
		}); !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			if _, err := b.CaptureAccess().Batch(captureDate, 2); err == nil {
				return fmt.Errorf("View or rollback committed")
			}
			return check(b.CaptureAccess(), 1)
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("pending_roundtrip_and_overlay", func(t *testing.T) {
		s := factory()
		in := orderedBatch(2)
		in.Segments = domain.CloneCaptureSegments(want)
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			out, err := b.ReceiveCaptures("phone", in)
			if err == nil && out.Status != "staged" {
				return fmt.Errorf("not staged")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		mutate(&in.Segments[0])
		pendingCheck := func(b *domain.Book) error {
			a := b.CaptureAccess()
			got, err := a.Pending(captureDate, "recording", 2, 0)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, want) {
				return fmt.Errorf("pending metadata differs")
			}
			mutate(&got[0])
			got, err = a.Pending(captureDate, "recording", 2, 0)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, want) {
				return fmt.Errorf("pending read aliased")
			}
			return nil
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			if err := pendingCheck(b); err != nil {
				return err
			}
			input := domain.CloneCaptureSegments(want)
			if err := b.CaptureAccess().PutPending(captureDate, "recording", 2, 0, input); err != nil {
				return err
			}
			mutate(&input[0])
			if err := pendingCheck(b); err != nil {
				return err
			}
			if _, err := b.ReceiveCaptures("phone", orderedBatch(1)); err != nil {
				return err
			}
			return check(b.CaptureAccess(), 2)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if err := pendingCheck(b); err != nil {
				return err
			}
			if _, err := b.ReceiveCaptures("phone", orderedBatch(1)); err != nil {
				return err
			}
			return check(b.CaptureAccess(), 2)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			pending, err := b.CaptureAccess().Pending(captureDate, "recording", 2, 0)
			if err != nil {
				return err
			}
			if len(pending) != 0 {
				return fmt.Errorf("drained pending survived")
			}
			return check(b.CaptureAccess(), 2)
		}); err != nil {
			t.Fatal(err)
		}
	})
	for _, action := range []string{"erase", "day", "expire", "purge"} {
		t.Run(action, func(t *testing.T) {
			s := factory()
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				if _, err := b.AppendCaptures(captureDate, "metadata", want); err != nil {
					return err
				}
				return b.CaptureAccess().PutPending(captureDate, "pending", 3, 0, want)
			}); err != nil {
				t.Fatal(err)
			}
			if action == "purge" {
				if _, err := s.BeginOwnerDeletion(ctx, "bl", "u", captureNow); err != nil {
					t.Fatal(err)
				}
				for range 100 {
					out, err := s.Purge(ctx, "bl", "u", 1, captureNow)
					if err != nil {
						t.Fatal(err)
					}
					if out.Status == "completed" {
						return
					}
				}
				t.Fatal("purge not completed")
			}
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				switch action {
				case "erase":
					_, err := b.Erase(captureDate, "1.0", "1.2", captureNow)
					return err
				case "day":
					_, err := b.EraseDay(captureDate, captureNow)
					return err
				default:
					expires, _ := domain.CaptureExpiresAt(captureDate, "UTC")
					return b.ExpireCaptures(captureDate, expires)
				}
			}); err != nil {
				t.Fatal(err)
			}
			// 擦除检查空位；删日与到期的物理残留另由两种存储的生命周期测试检查。
			if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
				a := b.CaptureAccess()
				if action == "expire" {
					_, err := a.Batch(captureDate, 1)
					if err == nil {
						return fmt.Errorf("cleanup still readable")
					}
					return nil
				}
				batch, err := a.Batch(captureDate, 1)
				if err != nil {
					return err
				}
				for _, g := range batch {
					if !g.Equal(domain.CaptureSegment{}) {
						return fmt.Errorf("metadata survived cleanup")
					}
				}
				pending, err := a.Pending(captureDate, "pending", 3, 0)
				if err != nil {
					return err
				}
				if len(pending) != 0 {
					return fmt.Errorf("pending survived cleanup")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
