// Package memoutbox 提供与内存日志、删除记录共用的提交锁。
package memoutbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"yanshi/internal/outbox"
)

type row struct {
	outbox.Envelope
	id  uint64
	xid uint64
}

type Store struct {
	Shard       int
	mu          sync.Mutex
	rows        []row
	ids         map[string]bool
	nextID      uint64
	nextXID     uint64
	state       outbox.StreamState
	checkpoints []checkpoint
	subs        map[string]map[chan struct{}]bool
}

func New() *Store {
	return &Store{ids: map[string]bool{}, state: outbox.NewStreamState(), subs: map[string]map[chan struct{}]bool{}}
}

var _ outbox.Store = (*Store)(nil)

// Commit 在信封锁内执行已准备好的提交；apply 必须在出错时不留下任何写入。
// 日志、队列或删除记录的锁由调用方持有到返回，读者不会看到半次提交。
func (s *Store) Commit(ctx context.Context, envelopes []outbox.Envelope, apply func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, e := range envelopes {
		if err := outbox.Validate(e); err != nil {
			return err
		}
		if seen[e.EnvelopeID] {
			return fmt.Errorf("outbox: duplicate envelope")
		}
		seen[e.EnvelopeID] = true
		if s.ids[e.EnvelopeID] {
			return fmt.Errorf("outbox: duplicate envelope")
		}
	}
	if err := apply(); err != nil {
		return err
	}
	s.nextXID++
	for _, e := range envelopes {
		s.ids[e.EnvelopeID] = true
		s.nextID++
		s.rows = append(s.rows, row{Envelope: e, id: s.nextID, xid: s.nextXID})
	}
	for bl := range envelopeLines(envelopes) {
		for ch := range s.subs[bl] {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

func (s *Store) List(ctx context.Context, businessLine string) ([]outbox.Envelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []outbox.Envelope
	for _, r := range s.rows {
		if r.BusinessLine == businessLine {
			result = append(result, r.Envelope)
		}
	}
	return result, nil
}

func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]row, 0, len(s.rows))
	for _, r := range s.rows {
		if r.SessionID != sessionID || r.Kind == outbox.SessionDeleted {
			kept = append(kept, r)
		} else {
			delete(s.ids, r.EnvelopeID)
		}
	}
	s.rows = kept
	return nil
}

type checkpoint struct {
	xmin uint64
	at   time.Time
}

func envelopeLines(envelopes []outbox.Envelope) map[string]bool {
	lines := map[string]bool{}
	for _, e := range envelopes {
		lines[e.BusinessLine] = true
	}
	return lines
}

// BindShard 仅在装配发布存储之前调用，不改变流的 epoch 或写入对象。
func (s *Store) BindShard(id int) { s.Shard = id }
