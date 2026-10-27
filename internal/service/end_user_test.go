package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

type duringEndUserDeletion func(context.Context) error

func (f duringEndUserDeletion) DeleteEndUser(ctx context.Context, _, _ string) error {
	return f(ctx)
}

func TestEndUserBarrierWithdrawsSessionAndAllowsReuseAfterGrace(t *testing.T) {
	ctx := context.Background()
	c := clock.Real{}
	log, q, d := memlog.New(), memqueue.New(c), lifecycle.NewMemDeletions()
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	x := lifecycle.NewMemIndex()
	svc := &service.Service{Store: &session.Store{Log: log, Waking: log.Waking(q), IDs: ids.Sequential("id"), Clock: c, Deletions: d},
		Agents: agents, Index: x, Deletions: d, Janitor: memqueue.New(c)}
	create := func() (string, error) {
		return svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	}
	req, err := svc.DeleteEndUser(ctx, "bl", "u", duringEndUserDeletion(func(ctx context.Context) error {
		if sid, err := create(); !errors.Is(err, service.ErrNotFound) || sid != "" {
			t.Fatalf("create during deletion = %q, %v", sid, err)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	barrier, err := d.GetEndUser(ctx, "bl", "u")
	if err != nil || len(barrier.Sessions) != 1 {
		t.Fatalf("barrier = %+v, %v", barrier, err)
	}
	sid := barrier.Sessions[0]
	if _, err := svc.Load(ctx, sid); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("withdrawn session readable: %v", err)
	}
	tomb, err := d.Get(ctx, sid)
	if err != nil || tomb.RequestID != req {
		t.Fatalf("withdrawal = %+v, %v", tomb, err)
	}
	if retry, err := svc.DeleteEndUser(ctx, "bl", "u"); err != nil || retry != req {
		t.Fatalf("retry = %s, %v", retry, err)
	}
	// 这里模拟 Janitor 已完成物理删除；宽限期内仍拒绝，清理到期屏障后允许重新使用 ID。
	now := c.Now()
	if err := log.Delete(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := x.Delete(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := d.Complete(ctx, sid, now); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteEndUser(ctx, barrier, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := create(); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("create in grace period = %v", err)
	}
	barrier, err = d.GetEndUser(ctx, "bl", "u")
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range barrier.Sessions {
		if err := log.Delete(ctx, sid); err != nil {
			t.Fatal(err)
		}
		if err := x.Delete(ctx, sid); err != nil {
			t.Fatal(err)
		}
		if err := d.Complete(ctx, sid, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.CompleteEndUser(ctx, barrier, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.PruneEndUsers(ctx, now.Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	if sid, err := create(); err != nil || sid == "" {
		t.Fatalf("create after grace = %q, %v", sid, err)
	}
}

type delayedSessionCreation struct {
	eventlog.Log
	beforeAppend func() error
}

func (l *delayedSessionCreation) Append(ctx context.Context, sid string, expected uint64, events ...*v1.Event) (uint64, error) {
	if hook := l.beforeAppend; hook != nil {
		l.beforeAppend = nil
		if err := hook(); err != nil {
			return 0, err
		}
	}
	return l.Log.Append(ctx, sid, expected, events...)
}

func TestEndUserDeletionCompletesBeforeInFlightSessionAppend(t *testing.T) {
	ctx := context.Background()
	c := clock.NewFake(time.Unix(1000, 0))
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	stores := app.Memory(c)
	log := &delayedSessionCreation{Log: stores.Log}
	stores.Log = log
	core := app.NewCore(stores, app.Options{Clock: c, Agents: agents})
	j := core.Janitor("j", nil, nil, 0)
	log.beforeAppend = func() error {
		if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err != nil {
			return err
		}
		if _, err := j.Step(ctx); err != nil {
			return err
		}
		return j.Sweep(ctx)
	}
	if sid, err := core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"}); sid != "" || !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("late append = %q, %v", sid, err)
	}
	b, err := stores.Deletions.GetEndUser(ctx, "bl", "u")
	if err != nil || b.CompletedAt.IsZero() || len(b.Sessions) != 1 {
		t.Fatalf("completed barrier = %+v, %v", b, err)
	}
	if events, err := eventlog.ReadAll(ctx, stores.Log, b.Sessions[0], 0); err != nil || len(events) != 0 {
		t.Fatalf("resurrected log = %v, %v", events, err)
	}
	if _, err := stores.Index.Get(ctx, b.Sessions[0]); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("resurrected index = %v", err)
	}
}
