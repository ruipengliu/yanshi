package memstore

import (
	"maps"
	"slices"

	"persona/domain"
)

type pendingKey struct {
	date, session   string
	seq, generation uint64
}

type pendingData map[pendingKey][]domain.CaptureSegment

func (t *captureTx) Pending(date, session string, seq, generation uint64) ([]domain.CaptureSegment, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	k := pendingKey{date, session, seq, generation}
	if v, ok := t.pendingWrites[k]; ok {
		return slices.Clone(v), nil
	}
	return slices.Clone(t.pendingBase[k]), nil
}

func (t *captureTx) PutPending(date, session string, seq, generation uint64, segments []domain.CaptureSegment) error {
	if err := t.check(); err != nil {
		return err
	}
	if t.pendingWrites == nil {
		t.pendingWrites = pendingData{}
	}
	t.pendingWrites[pendingKey{date, session, seq, generation}] = slices.Clone(segments)
	return nil
}

func (t *captureTx) ClearPending(date string) error {
	if err := t.check(); err != nil {
		return err
	}
	for _, data := range []pendingData{t.pendingBase, t.pendingWrites} {
		for k := range data {
			if k.date == date {
				if err := t.PutPending(k.date, k.session, k.seq, k.generation, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (t *captureTx) commitPending() pendingData {
	if len(t.pendingWrites) == 0 {
		return t.pendingBase
	}
	next := maps.Clone(t.pendingBase)
	if next == nil {
		next = pendingData{}
	}
	for k, v := range t.pendingWrites {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = v
		}
	}
	return next
}
