package sim

import (
	"context"
	"errors"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/outbox"
	"yanshi/internal/service"
)

// keyedCreateCheck 保留一个 (业务线, EndUser, 键) 的创建历史，日志清除后仍能发现重复写入。
type keyedCreateCheck struct {
	request service.CreateRequest
	writes  []string
}

type keyedCreateLog struct {
	eventlog.Log
	check        *keyedCreateCheck
	beforeAppend func(string) error
}

func (l keyedCreateLog) Append(ctx context.Context, sid string, expected uint64, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	for _, e := range events {
		if e.GetSessionCreated() == nil {
			continue
		}
		if l.beforeAppend != nil {
			if err := l.beforeAppend(sid); err != nil {
				return 0, err
			}
		}
		seq, err := l.Log.Append(ctx, sid, expected, envelopes, events...)
		if err == nil {
			l.check.writes = append(l.check.writes, sid)
		}
		return seq, err
	}
	return l.Log.Append(ctx, sid, expected, envelopes, events...)
}

func (w *World) attemptKeyedCreate(check *keyedCreateCheck, beforeAppend func(string) error) (string, error) {
	log := w.store.Log
	w.store.Log = keyedCreateLog{Log: log, check: check, beforeAppend: beforeAppend}
	defer func() { w.store.Log = log }()
	return w.svc.Create(context.Background(), check.request)
}

func (w *World) exerciseKeyedCreates() error {
	ctx := context.Background()
	for _, mode := range []string{"reply", "delete", "logout"} {
		check := keyedCreateCheck{request: service.CreateRequest{
			BusinessLine: "bl", EndUser: "sim-create-" + mode, Agent: "sim", IdempotencyKey: "intent",
		}}
		var beforeAppend func(string) error
		switch mode {
		case "delete":
			// 在索引已绑定、日志尚未写入时删除，迫使创建走写后复查。
			beforeAppend = func(sid string) error {
				if err := w.svc.Delete(ctx, sid, "end_user", ""); err != nil {
					return err
				}
				w.Stats.CreateDeletionRaces++
				return nil
			}
		case "logout":
			// 注销扫描索引后，迟到的创建才开始写入，必须通过屏障加入清理。
			w.endUserWriteBefore = true
			w.endUserWriteHook = func() error {
				user := check.request.EndUser
				if _, err := w.svc.DeleteEndUser(ctx, "bl", user); err != nil {
					return err
				}
				w.deletedUsers = append(w.deletedUsers, user)
				w.Stats.CreateEndUserDeletionRaces++
				return nil
			}
		}
		_, err := w.attemptKeyedCreate(&check, beforeAppend)
		w.endUserWriteHook = nil
		if len(check.writes) != 1 {
			return fmt.Errorf("invariant: keyed create (%s) wrote %d SessionCreated events: %v", mode, len(check.writes), err)
		}
		sid := check.writes[0]
		w.tracef("keyed create %s (%s)", sid, mode)
		if mode == "reply" {
			if err != nil {
				return fmt.Errorf("create before lost reply: %w", err)
			}
			// 客户端没有收到第一次的 ID，只能带原来的键重试。
			w.Stats.CreateRepliesLost++
			got, err := w.attemptKeyedCreate(&check, nil)
			if err != nil || got != sid || len(check.writes) != 1 {
				return fmt.Errorf("invariant: retry after lost create reply for %s returned %s, %v, writes %d", sid, got, err, len(check.writes))
			}
			w.Stats.CreateReplays++
			if err := w.svc.Delete(ctx, sid, "end_user", ""); err != nil {
				return err
			}
		} else if !errors.Is(err, service.ErrNotFound) {
			return fmt.Errorf("invariant: keyed create during %s returned %v, want not_found", mode, err)
		}
		if err := w.retryDeletedKeyedCreate(&check); err != nil {
			return err
		}
		w.deleted = append(w.deleted, sid)
		w.keyedCreates = append(w.keyedCreates, check)
	}
	return nil
}

func (w *World) retryDeletedKeyedCreate(check *keyedCreateCheck) error {
	got, err := w.attemptKeyedCreate(check, nil)
	if got != "" || !errors.Is(err, service.ErrNotFound) || len(check.writes) != 1 {
		return fmt.Errorf("invariant: retry of deleted keyed Session %s returned %s, %v, writes %d", check.writes[0], got, err, len(check.writes))
	}
	return nil
}

func (w *World) checkDeletedKeyedCreates() error {
	ctx := context.Background()
	for i := range w.keyedCreates {
		check := &w.keyedCreates[i]
		sid := check.writes[0]
		// 在 Janitor 删除日志和索引、注销屏障也已移除后，只有 Session 删除记录还能拦截旧键。
		if err := w.retryDeletedKeyedCreate(check); err != nil {
			return err
		}
		if events, err := w.log.Read(ctx, sid, 0, 1); err != nil || len(events) != 0 {
			return fmt.Errorf("invariant: deleted keyed Session %s has Events after retry: %d, %v", sid, len(events), err)
		}
		if _, err := w.index.Get(ctx, check.request.EndUser, sid); !errors.Is(err, lifecycle.ErrNotFound) {
			return fmt.Errorf("invariant: deleted keyed Session %s has index after retry: %v", sid, err)
		}
	}
	return nil
}
