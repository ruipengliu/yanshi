package memstore

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"

	"persona/domain"
	"persona/store"
)

type batchKey struct {
	date   string
	number uint64
}

// 已提交的批次及索引不可变，View 可共享它们；事务只复制实际改动的批次。
type captureData map[batchKey][]domain.CaptureSegment

type captureTx struct {
	pendingBase, pendingWrites pendingData
	ctx                        context.Context
	base                       captureData
	writes                     captureData
	closed                     bool
}

var _ domain.CaptureAccess = (*captureTx)(nil)

func newCaptureTx(ctx context.Context, base captureData, pending ...pendingData) *captureTx {
	t := &captureTx{ctx: ctx, base: base, writes: captureData{}}
	if len(pending) > 0 {
		t.pendingBase = pending[0]
	}
	return t
}

func (t *captureTx) check() error {
	if t.closed {
		return domain.ErrCaptureTransactionClosed
	}
	return t.ctx.Err()
}

func (t *captureTx) close() {
	t.closed = true
	t.pendingBase, t.pendingWrites = nil, nil
	t.base, t.writes, t.ctx = nil, nil, nil
}

func (t *captureTx) batch(k batchKey) ([]domain.CaptureSegment, bool) {
	if v, ok := t.writes[k]; ok {
		return v, v != nil
	}
	v, ok := t.base[k]
	return v, ok
}

func (t *captureTx) Batch(date string, number uint64) ([]domain.CaptureSegment, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	v, ok := t.batch(batchKey{date, number})
	if !ok {
		return nil, fmt.Errorf("capture batch does not exist")
	}
	store.RecordLoad(t.ctx, 0, len(v))
	return slices.Clone(v), nil
}

func (t *captureTx) Lookup(date string, ref domain.Ref) (domain.CaptureSegment, bool, error) {
	if err := t.check(); err != nil {
		return domain.CaptureSegment{}, false, err
	}
	v, _ := t.batch(batchKey{date, ref.Seq})
	if ref.Index < 0 || ref.Index >= len(v) || v[ref.Index].Text == "" {
		return domain.CaptureSegment{}, false, nil
	}
	store.RecordLoad(t.ctx, 0, 1)
	return v[ref.Index], true, nil
}

func refLess(a, b domain.Ref) bool {
	return a.Seq < b.Seq || a.Seq == b.Seq && a.Index < b.Index
}

func (t *captureTx) numbers(date string, from, to domain.Ref) []uint64 {
	numbers := map[uint64]bool{}
	for _, data := range []captureData{t.base, t.writes} {
		for k := range data {
			if k.date == date && (from.Seq == 0 || k.number >= from.Seq) && (to.Seq == 0 || k.number <= to.Seq) {
				numbers[k.number] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(numbers))
}

func (t *captureTx) Scan(date string, from, to domain.Ref) iter.Seq2[domain.CaptureRecord, error] {
	return func(yield func(domain.CaptureRecord, error) bool) {
		if err := t.check(); err != nil {
			yield(domain.CaptureRecord{}, err)
			return
		}
		for _, number := range t.numbers(date, from, to) {
			v, _ := t.batch(batchKey{date, number})
			for i, g := range v {
				if err := t.check(); err != nil {
					yield(domain.CaptureRecord{}, err)
					return
				}
				r := domain.Ref{Seq: number, Index: i}
				if g.Text == "" || from.Seq != 0 && refLess(r, from) || to.Seq != 0 && refLess(to, r) {
					continue
				}
				store.RecordLoad(t.ctx, 0, 1)
				if !yield(domain.CaptureRecord{Ref: r, Segment: g}, nil) {
					return
				}
				// Pull2 可把迭代暂停在最后一次 yield；恢复时也须先检查事务寿命。
				if err := t.check(); err != nil {
					yield(domain.CaptureRecord{}, err)
					return
				}
			}
		}
	}
}

func (t *captureTx) Adjacent(date string, ref domain.Ref, before bool) (domain.CaptureRecord, bool, error) {
	if err := t.check(); err != nil {
		return domain.CaptureRecord{}, false, err
	}
	from, to := domain.Ref{}, domain.Ref{}
	if before {
		to = ref
	} else {
		from = ref
	}
	numbers := t.numbers(date, from, to)
	if before {
		slices.Reverse(numbers)
	}
	for _, number := range numbers {
		v, _ := t.batch(batchKey{date, number})
		for n := range len(v) {
			i := n
			if before {
				i = len(v) - 1 - n
			}
			r := domain.Ref{Seq: number, Index: i}
			if v[i].Text == "" || ref.Seq != 0 && (before && !refLess(r, ref) || !before && !refLess(ref, r)) {
				continue
			}
			store.RecordLoad(t.ctx, 0, 1)
			return domain.CaptureRecord{Ref: r, Segment: v[i]}, true, nil
		}
	}
	return domain.CaptureRecord{}, false, nil
}

func (t *captureTx) Append(date string, number uint64, segments []domain.CaptureSegment) error {
	if err := t.check(); err != nil {
		return err
	}
	k := batchKey{date, number}
	if _, exists := t.batch(k); exists {
		return fmt.Errorf("capture batch already exists")
	}
	if number == 0 || len(segments) == 0 {
		return fmt.Errorf("invalid capture batch")
	}
	t.writes[k] = slices.Clone(segments)
	return nil
}

func (t *captureTx) Erase(date string, from, to domain.Ref) error {
	if err := t.check(); err != nil {
		return err
	}
	for _, number := range t.numbers(date, from, to) {
		k := batchKey{date, number}
		v, _ := t.batch(k)
		var changed []domain.CaptureSegment
		for i, g := range v {
			r := domain.Ref{Seq: number, Index: i}
			if g == (domain.CaptureSegment{}) || from.Seq != 0 && refLess(r, from) || to.Seq != 0 && refLess(to, r) {
				continue
			}
			if changed == nil {
				changed = slices.Clone(v)
			}
			changed[i] = domain.CaptureSegment{}
		}
		if changed != nil {
			t.writes[k] = changed
		}
	}
	return nil
}

func (t *captureTx) commit() captureData {
	if len(t.writes) == 0 {
		return t.base
	}
	next := maps.Clone(t.base)
	if next == nil {
		next = captureData{}
	}
	for key, value := range t.writes {
		if value == nil {
			delete(next, key)
		} else {
			next[key] = value
		}
	}
	return next
}

func (t *captureTx) DeleteDay(date string) error {
	if err := t.check(); err != nil {
		return err
	}
	for _, number := range t.numbers(date, domain.Ref{}, domain.Ref{}) {
		t.writes[batchKey{date, number}] = nil
	}
	return nil
}

func (t *captureTx) Check() error { return t.check() }
