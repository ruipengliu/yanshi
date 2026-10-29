package storetest

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"persona/domain"
	"persona/store"
)

// 少量批次看不出文本排序；240 段同时跨过 9/10、99/100 和存储预读页的 128 条边界。
func captureNumericOrder(t *testing.T, factory func() store.Store) {
	s := factory()
	ctx := context.Background()
	var want []domain.CaptureRecord
	check := func(a domain.CaptureAccess) error {
		checkScan := func(name string, from, to domain.Ref, expected []domain.CaptureRecord) error {
			n := 0
			for record, err := range a.Scan(captureDate, from, to) {
				if err != nil {
					return err
				}
				if n >= len(expected) || !reflect.DeepEqual(record, expected[n]) {
					return fmt.Errorf("%s: unexpected record %d: %+v", name, n, record)
				}
				n++
			}
			if n != len(expected) {
				return fmt.Errorf("%s: got %d records, want %d", name, n, len(expected))
			}
			return nil
		}
		if err := checkScan("all", domain.Ref{}, domain.Ref{}, want); err != nil {
			return err
		}
		for _, bounds := range []struct {
			name        string
			first, last int
		}{
			{"across_9_10", 17, 21},
			{"across_99_100", 197, 201},
			{"middle_reference", 122, len(want)},
			{"before_prefetch_boundary", 126, 131},
			{"at_prefetch_boundary", 127, 130},
			{"after_prefetch_boundary", 128, len(want)},
			{"exact_prefetch_page", 0, 128},
			{"bounded_multiple_pages", 9, len(want) - 1},
		} {
			if err := checkScan(bounds.name, want[bounds.first].Ref, want[bounds.last-1].Ref, want[bounds.first:bounds.last]); err != nil {
				return err
			}
		}
		// 消费者提前停止再从下一引用续读，不能因预读了 128 条而丢掉尚未交出的段。
		for _, size := range []int{1, 127, 128, 129} {
			consumed := 0
			for consumed < len(want) {
				start := consumed
				for record, err := range a.Scan(captureDate, want[start].Ref, domain.Ref{}) {
					if err != nil {
						return err
					}
					if consumed >= len(want) || !reflect.DeepEqual(record, want[consumed]) {
						return fmt.Errorf("chunk %d: unexpected record %d: %+v", size, consumed, record)
					}
					consumed++
					if consumed-start == size {
						break
					}
				}
				if consumed == start {
					return fmt.Errorf("chunk %d: scan stopped at %d", size, start)
				}
			}
		}
		for i, expected := range want {
			segment, found, err := a.Lookup(captureDate, expected.Ref)
			if err != nil {
				return err
			}
			if !found || !reflect.DeepEqual(segment, expected.Segment) {
				return fmt.Errorf("lookup %s differs", expected.Ref)
			}
			for _, before := range []bool{false, true} {
				next := i + 1
				if before {
					next = i - 1
				}
				record, found, err := a.Adjacent(captureDate, expected.Ref, before)
				if err != nil {
					return err
				}
				exists := next >= 0 && next < len(want)
				if found != exists || exists && !reflect.DeepEqual(record, want[next]) {
					return fmt.Errorf("adjacent %s before=%t: %+v, found=%t", expected.Ref, before, record, found)
				}
			}
		}
		for _, before := range []bool{false, true} {
			expected := want[0]
			if before {
				expected = want[len(want)-1]
			}
			record, found, err := a.Adjacent(captureDate, domain.Ref{}, before)
			if err != nil {
				return err
			}
			if !found || !reflect.DeepEqual(record, expected) {
				return fmt.Errorf("zero adjacent before=%t: %+v, found=%t", before, record, found)
			}
		}
		for _, ref := range []domain.Ref{{Seq: 10, Index: 99}, {Seq: 121}} {
			if _, found, err := a.Lookup(captureDate, ref); err != nil || found {
				return fmt.Errorf("missing lookup %s: found=%t, error=%v", ref, found, err)
			}
		}
		return nil
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		for seq := 1; seq <= 120; seq++ {
			segments := make([]domain.CaptureSegment, 1+(seq-1)%3)
			for i := range segments {
				segments[i] = domain.CaptureSegment{Speaker: "wearer", Text: fmt.Sprintf("批次 %d 段 %d", seq, i)}
				want = append(want, domain.CaptureRecord{Ref: domain.Ref{Seq: uint64(seq), Index: i}, Segment: segments[i]})
			}
			if _, err := b.AppendCaptures(captureDate, fmt.Sprintf("batch-%d", seq), segments); err != nil {
				return err
			}
		}
		return check(b.CaptureAccess())
	}); err != nil {
		t.Fatal("before commit:", err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error { return check(b.CaptureAccess()) }); err != nil {
		t.Fatal("after commit:", err)
	}
}
