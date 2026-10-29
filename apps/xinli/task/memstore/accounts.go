package memstore

import (
	"context"
	"sort"

	"xinli/account"
	"xinli/task"
)

type gateKey struct{}
type heldGate struct {
	store *Store
	owner task.Owner
}
type ownerGate struct {
	token chan struct{}
	refs  int
}

func (s *Store) WithOwner(ctx context.Context, owner task.Owner, fn func(context.Context) error) error {
	if v, ok := ctx.Value(gateKey{}).(heldGate); ok && v.store == s {
		if v.owner != owner {
			return account.ErrInvalid
		}
		return fn(ctx)
	}
	s.gatesMu.Lock()
	gate := s.gates[owner]
	if gate == nil {
		gate = &ownerGate{token: make(chan struct{}, 1)}
		s.gates[owner] = gate
	}
	gate.refs++
	s.gatesMu.Unlock()
	defer func() {
		s.gatesMu.Lock()
		defer s.gatesMu.Unlock()
		gate.refs--
		if gate.refs == 0 {
			delete(s.gates, owner)
		}
	}()
	select {
	case gate.token <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate.token }()
	return fn(context.WithValue(ctx, gateKey{}, heldGate{s, owner}))
}
func (s *Store) Register(ctx context.Context, identity string, a account.Account) (account.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return account.Account{}, err
	}
	if owner, ok := s.identities[identity]; ok {
		return s.accounts[owner].Account, nil
	}
	if _, exists := s.accounts[a.Owner]; exists {
		return account.Account{}, account.ErrInvalid
	}
	s.accounts[a.Owner] = account.Record{Account: a, Devices: map[string]account.Device{}, Sessions: map[string]account.Session{}}
	s.identities[identity] = a.Owner
	return a, nil
}
func (s *Store) ReadAccount(ctx context.Context, owner task.Owner) (account.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return account.Record{}, err
	}
	r, ok := s.accounts[owner]
	if !ok {
		return account.Record{}, account.ErrNotFound
	}
	return account.Clone(r), nil
}
func (s *Store) UpdateAccount(ctx context.Context, owner task.Owner, fn func(*account.Record) error) error {
	return s.WithOwner(ctx, owner, func(ctx context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		old, ok := s.accounts[owner]
		if !ok {
			return account.ErrNotFound
		}
		next := account.Clone(old)
		if err := fn(&next); err != nil {
			return err
		}
		if !account.ValidChange(old, next) {
			return account.ErrInvalid
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		ctx = context.WithoutCancel(ctx)
		// 先验证全部再修改；失败回调不泄露半份账号或任务。
		if next.Cleanup {
			delete(s.deliveries, owner)
			for key, cp := range s.recoverySessions {
				if key[0] == owner.BusinessLine && cp.EndUser == owner.EndUser {
					s.forgetRecovery(owner.BusinessLine, cp.SessionID)
				}
			}
			for id, c := range s.creations {
				if c.Owner == owner {
					delete(s.creations, id)
				}
			}
			for id, t := range s.tasks {
				if t.Owner == owner {
					delete(s.tasks, id)
				}
			}
			for k := range s.sources {
				if k.Owner == owner {
					delete(s.sources, k)
				}
			}
			for id, key := range s.identities {
				if key == owner {
					delete(s.identities, id)
				}
			}
		}
		for _, t := range next.Enqueue {
			if err := s.receiveLocked(ctx, t.Source, []task.Spec{t.Spec}, t.RetryAt, true); err != nil {
				return err
			}
		}
		s.accounts[owner] = account.Clone(next)
		return nil
	})
}
func (s *Store) TokenOwner(ctx context.Context, hash string) (task.Owner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Owner{}, err
	}
	for owner, r := range s.accounts {
		for _, ss := range r.Sessions {
			if ss.AccessHash == hash || ss.RefreshHash == hash {
				return owner, nil
			}
		}
	}
	return task.Owner{}, account.ErrNotFound
}

var _ account.Store = (*Store)(nil)

func (s *Store) DeletionOwner(ctx context.Context, hash string) (task.Owner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return task.Owner{}, err
	}
	for owner, r := range s.accounts {
		if hash != "" && r.Deletion.ReceiptHash == hash {
			return owner, nil
		}
	}
	return task.Owner{}, account.ErrNotFound
}

func (s *Store) AccountOwners(ctx context.Context, line, after string, limit int) ([]task.Owner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if line == "" || limit < 1 || limit > 200 {
		return nil, account.ErrInvalid
	}
	out := []task.Owner{}
	for owner := range s.accounts {
		if owner.BusinessLine == line && owner.EndUser > after {
			out = append(out, owner)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndUser < out[j].EndUser })
	return out[:min(limit, len(out))], nil
}

var _ account.Enumerator = (*Store)(nil)
