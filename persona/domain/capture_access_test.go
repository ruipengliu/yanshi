package domain

import (
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 领域测试显式提供原文，不能让 NewBook/Clone 暗中携带一个内存存储。
// 真正的事务隔离与寿命由 storetest 在各存储实现上验证。
type testCaptures map[string]map[uint64][]CaptureSegment

func newCaptureTestBook() *Book {
	b := NewBook()
	b.SetCaptureClock(func() time.Time { return captureTestNow })
	b.SetCaptureAccess(testCaptures{})
	return b
}

func cloneCaptureTestBook(b *Book) (*Book, error) {
	out, err := b.Clone()
	if err != nil {
		return nil, err
	}
	data := testCaptures{}
	for date, batches := range b.captures.(testCaptures) {
		data[date] = map[uint64][]CaptureSegment{}
		for n, segments := range batches {
			data[date][n] = CloneCaptureSegments(segments)
		}
	}
	out.SetCaptureAccess(data)
	out.SetCaptureClock(b.captureClock)
	return out, nil
}

func dayCapturesTest(t *testing.T, b *Book, date string) []CaptureView {
	t.Helper()
	out, err := b.DayCaptures(date)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func containsRefTest(t *testing.T, b *Book, date string, ref Ref) bool {
	t.Helper()
	found, err := b.containsRef(date, ref)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func (m testCaptures) Batch(date string, number uint64) ([]CaptureSegment, error) {
	v, ok := m[date][number]
	if !ok {
		return nil, fmt.Errorf("missing batch")
	}
	return CloneCaptureSegments(v), nil
}

func (m testCaptures) Lookup(date string, ref Ref) (CaptureSegment, bool, error) {
	v := m[date][ref.Seq]
	if ref.Index < 0 || ref.Index >= len(v) || v[ref.Index].Text == "" {
		return CaptureSegment{}, false, nil
	}
	return v[ref.Index].Clone(), true, nil
}

func (m testCaptures) Scan(date string, from, to Ref) iter.Seq2[CaptureRecord, error] {
	return func(yield func(CaptureRecord, error) bool) {
		for _, number := range slices.Sorted(maps.Keys(m[date])) {
			for i, g := range m[date][number] {
				r := Ref{Seq: number, Index: i}
				if g.Text == "" || from.Seq != 0 && !refLE(from, r) || to.Seq != 0 && !refLE(r, to) {
					continue
				}
				if !yield(CaptureRecord{Ref: r, Segment: g.Clone()}, nil) {
					return
				}
			}
		}
	}
}

func (m testCaptures) Adjacent(date string, ref Ref, before bool) (CaptureRecord, bool, error) {
	var out CaptureRecord
	found := false
	for r := range m.Scan(date, Ref{}, Ref{}) {
		if ref.Seq != 0 && (before && refLE(ref, r.Ref) || !before && refLE(r.Ref, ref)) {
			continue
		}
		out, found = r, true
		if !before {
			break
		}
	}
	return out, found, nil
}

func (m testCaptures) Append(date string, number uint64, segments []CaptureSegment) error {
	if m[date] == nil {
		m[date] = map[uint64][]CaptureSegment{}
	}
	if _, exists := m[date][number]; exists {
		return fmt.Errorf("duplicate batch")
	}
	m[date][number] = CloneCaptureSegments(segments)
	return nil
}

func (m testCaptures) Erase(date string, from, to Ref) error {
	for number, segments := range m[date] {
		for i := range segments {
			r := Ref{Seq: number, Index: i}
			if (from.Seq == 0 || refLE(from, r)) && (to.Seq == 0 || refLE(r, to)) {
				segments[i] = CaptureSegment{}
			}
		}
	}
	return nil
}

func (m testCaptures) DeleteDay(date string) error { delete(m, date); return nil }

func (m testCaptures) Check() error { return nil }

// 暂存使用独立命名空间，不能被正式原文 Scan 或 Batch 读出。
func testPendingDate(date, session string, generation uint64) string {
	return date + "\x00" + strconv.FormatUint(generation, 10) + "\x00" + session
}

func (m testCaptures) Pending(date, session string, seq, generation uint64) ([]CaptureSegment, error) {
	return CloneCaptureSegments(m[testPendingDate(date, session, generation)][seq]), nil
}

func (m testCaptures) PutPending(date, session string, seq, generation uint64, segments []CaptureSegment) error {
	key := testPendingDate(date, session, generation)
	if segments == nil {
		delete(m[key], seq)
		return nil
	}
	if m[key] == nil {
		m[key] = map[uint64][]CaptureSegment{}
	}
	m[key][seq] = CloneCaptureSegments(segments)
	return nil
}

func (m testCaptures) ClearPending(date string) error {
	for key := range m {
		if strings.HasPrefix(key, date+"\x00") {
			delete(m, key)
		}
	}
	return nil
}
