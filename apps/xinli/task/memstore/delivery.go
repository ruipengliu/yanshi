package memstore

import (
	"context"
	"sort"
	"strconv"
	"time"

	"xinli/account"
	"xinli/delivery"
	"xinli/kernel"
	"xinli/recovery"
	"xinli/task"
)

func (s *Store) Pending(ctx context.Context, line string, limit int) ([]kernel.Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, task.ErrInvalid
	}
	out := []kernel.Envelope{}
	for key, e := range s.recoveryReceipts {
		if key[0] == line && !s.deliveryProcessed[key] {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].Seq < out[j].Seq
	})
	return out[:min(limit, len(out))], nil
}

func (s *Store) Process(ctx context.Context, e kernel.Envelope, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := [3]string{e.BusinessLine, e.SessionID, strconv.FormatUint(e.Seq, 10)}
	stored, exists := s.recoveryReceipts[key]
	if !exists || s.deliveryProcessed[key] {
		return nil
	}
	e = stored
	if s.deliveryProcessed == nil {
		s.deliveryProcessed = map[[3]string]bool{}
	}
	owner := task.Owner{BusinessLine: e.BusinessLine, EndUser: e.EndUser}
	if s.accounts[owner].Account.Status == account.Active && !s.recoverySessions[[2]string{e.BusinessLine, e.SessionID}].Gone {
		state := delivery.Clone(s.deliveries[owner])
		state.Receive(e, now)
		if s.deliveries == nil {
			s.deliveries = map[task.Owner]delivery.State{}
		}
		s.deliveries[owner] = state
	}
	if e.Kind == "run_completed" || e.Kind == "run_failed" {
		for id, t := range s.tasks {
			if t.Owner == owner && t.Session == e.SessionID && (t.State == task.Accepted || t.State == task.Unknown) && (t.Receipt.RunID == "" || t.Receipt.RunID == e.RunID) {
				// 信封只提前安排安全对账，不能据此跳过写调用结果核验。
				t.RetryAt = now
				s.tasks[id] = t
			}
		}
	}
	s.deliveryProcessed[key] = true
	return nil
}

func (s *Store) ReadDelivery(ctx context.Context, owner task.Owner) (delivery.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return delivery.State{}, err
	}
	if s.accounts[owner].Account.Status != account.Active {
		return delivery.State{}, account.ErrInactive
	}
	return delivery.Clone(s.deliveries[owner]), nil
}

func (s *Store) UpdateDelivery(ctx context.Context, owner task.Owner, fn func(*delivery.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.accounts[owner].Account.Status != account.Active {
		return account.ErrInactive
	}
	next := delivery.Clone(s.deliveries[owner])
	if err := fn(&next); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 回调前读到的任务不能在删除提交后重新插入。
	for key, cp := range s.recoverySessions {
		if key[0] == owner.BusinessLine && cp.Gone {
			next.Forget(cp.SessionID)
		}
	}
	s.initRecovery()
	for _, id := range next.Inbound {
		key := [2]string{owner.BusinessLine, id}
		if _, exists := s.recoverySessions[key]; !exists {
			s.recoverySessions[key] = recovery.Checkpoint{SessionID: id, EndUser: owner.EndUser}
		}
	}
	if s.deliveries == nil {
		s.deliveries = map[task.Owner]delivery.State{}
	}
	s.deliveries[owner] = delivery.Clone(next)
	return nil
}

var _ delivery.Store = (*Store)(nil)
