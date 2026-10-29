package memstore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	"xinli/account"
	"xinli/kernel"
	"xinli/recovery"
	"xinli/task"
)

// 恢复状态与记录型消费者共享提交边界；B6 在此边界内接入收件箱和任务。
func (s *Store) initRecovery() {
	if s.recoveryProgress == nil {
		s.recoveryProgress = map[string]recovery.Progress{}
		s.recoverySessions = map[[2]string]recovery.Checkpoint{}
		s.recoveryReceipts = map[[3]string]kernel.Envelope{}
	}
}

func copyRecovery[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *Store) Load(ctx context.Context, line string) (recovery.Progress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return recovery.Progress{}, err
	}
	p := copyRecovery(s.recoveryProgress[recovery.ProgressKey(ctx, line)])
	p.BusinessLine = line
	return p, nil
}

func (s *Store) Session(ctx context.Context, line, id string) (recovery.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return recovery.Checkpoint{}, err
	}
	cp := copyRecovery(s.recoverySessions[[2]string{line, id}])
	cp.SessionID = id
	return cp, nil
}

func (s *Store) Known(ctx context.Context, line string, round uint64, after string, limit int) ([]recovery.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, errors.New("recovery: invalid limit")
	}
	// 升级到 B5 前已有的任务和创建意图也是已知项，不能仅认新的接收表。
	known := map[string]recovery.Checkpoint{}
	for _, t := range s.tasks {
		if t.BusinessLine == line && t.Session != "" {
			known[t.Session] = recovery.Checkpoint{SessionID: t.Session, EndUser: t.EndUser}
		}
	}
	for _, c := range s.creations {
		if c.BusinessLine == line && c.Session != "" {
			known[c.Session] = recovery.Checkpoint{SessionID: c.Session, EndUser: c.EndUser}
		}
	}
	for key, cp := range s.recoverySessions {
		if key[0] == line {
			known[cp.SessionID] = cp
		}
	}
	out := []recovery.Checkpoint{}
	for _, cp := range known {
		owner := task.Owner{BusinessLine: line, EndUser: cp.EndUser}
		if cp.SessionID > after && !cp.Gone && (recovery.Shard(ctx) >= 0 || cp.SeenRound != round) && s.accounts[owner].Account.Status != account.Deleted {
			out = append(out, copyRecovery(cp))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out[:min(limit, len(out))], nil
}

func (s *Store) Commit(ctx context.Context, expected uint64, p recovery.Progress, batch recovery.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.initRecovery()
	if s.recoveryProgress[recovery.ProgressKey(ctx, p.BusinessLine)].Version != expected || p.Version != expected+1 {
		return recovery.ErrConflict
	}
	if err := recovery.ValidateBatch(p.BusinessLine, batch); err != nil {
		return err
	}
	for _, e := range batch.Envelopes {
		if e.EnvelopeID == "" {
			continue
		}
		for key, old := range s.recoveryReceipts {
			if key[0] == p.BusinessLine && old.EnvelopeID == e.EnvelopeID && (old.SessionID != e.SessionID || old.Seq != e.Seq) {
				return errors.New("recovery: envelope identity conflict")
			}
		}
	}
	// 注销清理与接收共用这把锁，迟到的页不能复活账号的应用数据。
	deleted := func(user string) bool {
		return s.accounts[task.Owner{BusinessLine: p.BusinessLine, EndUser: user}].Account.Status == account.Deleted
	}
	if cp := batch.Checkpoint; cp != nil {
		key := [2]string{p.BusinessLine, cp.SessionID}
		if !s.recoverySessions[key].Gone && !deleted(cp.EndUser) {
			s.recoverySessions[key] = copyRecovery(*cp)
		}
	}
	for _, e := range batch.Envelopes {
		key := [2]string{p.BusinessLine, e.SessionID}
		if e.Kind == "session_deleted" {
			s.forgetRecovery(p.BusinessLine, e.SessionID)
		} else {
			cp := s.recoverySessions[key]
			if cp.Gone || deleted(e.EndUser) {
				continue
			}
			cp.SessionID, cp.EndUser = e.SessionID, e.EndUser
			s.recoverySessions[key] = cp
		}
		rk := [3]string{p.BusinessLine, e.SessionID, strconv.FormatUint(e.Seq, 10)}
		if old, ok := s.recoveryReceipts[rk]; !ok || old.EnvelopeID == "" {
			s.recoveryReceipts[rk] = e
		}
	}
	if batch.Inaccessible != "" {
		s.forgetRecovery(p.BusinessLine, batch.Inaccessible)
	}
	s.recoveryProgress[recovery.ProgressKey(ctx, p.BusinessLine)] = copyRecovery(p)
	return nil
}

func (s *Store) forgetRecovery(line, id string) {
	for owner, state := range s.deliveries {
		if owner.BusinessLine == line {
			state.Forget(id)
			s.deliveries[owner] = state
		}
	}
	s.recoverySessions[[2]string{line, id}] = recovery.Checkpoint{SessionID: id, Gone: true}
	for key, e := range s.recoveryReceipts {
		if key[0] == line && e.SessionID == id && e.Kind != "session_deleted" {
			delete(s.recoveryReceipts, key)
			delete(s.deliveryProcessed, key)
		}
	}
}

// Recorded 供 B5 验证接收结果；它不发送通知，也不代替 B6 的收件箱。
func (s *Store) Recorded(ctx context.Context, line string) ([]kernel.Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []kernel.Envelope{}
	for key, e := range s.recoveryReceipts {
		if key[0] == line {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

var _ recovery.Consumer = (*Store)(nil)
