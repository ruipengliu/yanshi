package memoutbox

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/outbox"
)

func (s *Store) Read(ctx context.Context, req outbox.Request) (outbox.Page, error) {
	r, err := req.Normalize()
	if req.Shard != s.Shard {
		err = outbox.ErrShard
	}
	if err != nil {
		return outbox.Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return outbox.Page{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	start, err := s.state.Start(r)
	if err != nil {
		return outbox.Page{}, err
	}
	watermark := outbox.Position{XID: s.nextXID + 1}
	if watermark.Before(start) {
		return outbox.Page{}, outbox.ErrCursor
	}
	page := outbox.Page{Events: []outbox.Envelope{}}
	next := watermark
	for _, row := range s.rows {
		pos := outbox.Position{XID: row.xid, ID: row.id}
		if !start.Before(pos) || row.BusinessLine != r.BusinessLine || !slices.Contains(r.Kinds, row.Kind) {
			continue
		}
		page.Events = append(page.Events, row.Envelope)
		if len(page.Events) == r.Limit {
			next = pos
			break
		}
	}
	page.Next = s.state.Encode(r, next)
	metrics.OutboxConsumerLag.WithLabelValues(strconv.Itoa(s.Shard), r.BusinessLine).Set(float64(watermark.XID - next.XID))
	return page, nil
}

func (s *Store) Head(ctx context.Context, req outbox.Request) (string, error) {
	r, err := req.Normalize()
	if req.Shard != s.Shard {
		err = outbox.ErrShard
	}
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.state.Start(r); err != nil {
		return "", err
	}
	return s.state.Encode(r, outbox.Position{XID: s.nextXID + 1}), nil
}

func (s *Store) Subscribe(bl string) (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{}, 1)
	if s.subs[bl] == nil {
		s.subs[bl] = map[chan struct{}]bool{}
	}
	s.subs[bl][ch] = true
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs[bl], ch)
		if len(s.subs[bl]) == 0 {
			delete(s.subs, bl)
		}
	}
}

func (s *Store) Maintain(ctx context.Context, clk clock.Clock, batch int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if batch <= 0 {
		return outbox.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	xmin := s.nextXID + 1
	now := clk.Now()
	if len(s.checkpoints) == 0 || s.checkpoints[len(s.checkpoints)-1].xmin != xmin {
		s.checkpoints = append(s.checkpoints, checkpoint{xmin: xmin, at: now})
	}
	if s.state.Target.XID == 0 {
		for _, cp := range s.checkpoints {
			if !cp.at.After(now.Add(-outbox.Retention)) && cp.xmin > s.state.Floor.XID {
				s.state.Target = outbox.Position{XID: cp.xmin}
			}
		}
	}
	if s.state.Target.XID == 0 {
		return nil
	}
	kept := s.rows[:0]
	removed, remaining := 0, false
	for _, row := range s.rows {
		if row.xid < s.state.Target.XID {
			if removed < batch {
				delete(s.ids, row.EnvelopeID)
				removed++
				continue
			}
			remaining = true
		}
		kept = append(kept, row)
	}
	s.rows = kept
	if !remaining {
		s.state.Floor = s.state.Target
		s.state.Target = outbox.Position{}
		s.checkpoints = slices.DeleteFunc(s.checkpoints, func(cp checkpoint) bool { return cp.xmin <= s.state.Floor.XID })
	}
	return nil
}

// RotateEpoch 只用于无法保证流连续性的重建；普通进程重启不能换世代。
func (s *Store) RotateEpoch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = outbox.NewStreamState()
	s.checkpoints = nil
	return nil
}

func (s *Store) Stats(ctx context.Context) (outbox.Stats, error) {
	if err := ctx.Err(); err != nil {
		return outbox.Stats{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var size int64
	for _, row := range s.rows {
		b, _ := json.Marshal(row.Envelope)
		size += int64(len(b))
	}
	return outbox.Stats{Rows: int64(len(s.rows)), Bytes: size}, nil
}
