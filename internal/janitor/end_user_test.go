package janitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/service"
)

type failedEndUserRemoval struct{}

func (failedEndUserRemoval) DeleteEndUser(context.Context, string, string) error {
	return errors.New("removal failed")
}

func TestEndUserGraceStartsAfterAllCleanup(t *testing.T) {
	ctx := context.Background()
	c := clock.NewFake(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	core := app.NewCore(app.Memory(c), app.Options{Clock: c, Agents: agents, EndUserGrace: time.Hour})
	svc, d := core.Service, core.Stores.Deletions
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	// 已单独删除的 Session 不属于这次 Request，仍须等它清理完成。
	if err := svc.Delete(ctx, sid, "end_user", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteEndUser(ctx, "bl", "u", failedEndUserRemoval{}); err == nil {
		t.Fatal("failed remover accepted")
	}
	b, err := d.GetEndUser(ctx, "bl", "u")
	if err != nil {
		t.Fatal(err)
	}
	requestID := b.RequestID
	j := core.Janitor("j", nil, nil, 0)
	c.Advance(72 * time.Hour)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	b, err = d.GetEndUser(ctx, "bl", "u")
	if err != nil || b.Sealed || !b.CompletedAt.IsZero() {
		t.Fatalf("failed deletion = %+v, %v", b, err)
	}
	if retry, err := svc.DeleteEndUser(ctx, "bl", "u"); err != nil || retry != requestID {
		t.Fatalf("retry = %s, %v", retry, err)
	}
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	b, err = d.GetEndUser(ctx, "bl", "u")
	if err != nil || !b.CompletedAt.IsZero() {
		t.Fatalf("completed before session cleanup = %+v, %v", b, err)
	}
	for range 10 {
		did, err := j.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !did {
			break
		}
	}
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	b, err = d.GetEndUser(ctx, "bl", "u")
	if err != nil || !b.CompletedAt.Equal(c.Now()) || !b.ExpiresAt.Equal(c.Now().Add(time.Hour)) {
		t.Fatalf("grace = %+v, %v", b, err)
	}
	c.Advance(time.Hour)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetEndUser(ctx, "bl", "u"); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("expired barrier = %v", err)
	}
	if _, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"}); err != nil {
		t.Fatalf("reuse after grace = %v", err)
	}
}

type failSessionMark struct {
	lifecycle.Deletions
	fail bool
}

func (d *failSessionMark) Mark(ctx context.Context, t *lifecycle.Tombstone) (bool, error) {
	if d.fail {
		d.fail = false
		return false, errors.New("session mark failed")
	}
	return d.Deletions.Mark(ctx, t)
}

func TestEndUserJanitorRepairsFailedSessionMark(t *testing.T) {
	ctx := context.Background()
	c := clock.NewFake(time.Unix(1000, 0))
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	stores := app.Memory(c)
	d := &failSessionMark{Deletions: stores.Deletions}
	stores.Deletions = d
	core := app.NewCore(stores, app.Options{Clock: c, Agents: agents})
	if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	d.fail = true
	if _, err := core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"}); err == nil {
		t.Fatal("creation accepted despite the barrier")
	}
	b, err := d.GetEndUser(ctx, "bl", "u")
	if err != nil || len(b.Sessions) != 1 {
		t.Fatalf("tracked session = %+v, %v", b, err)
	}
	if _, err := d.Get(ctx, b.Sessions[0]); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("failed mark unexpectedly persisted: %v", err)
	}
	j := core.Janitor("j", nil, nil, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	b, err = d.GetEndUser(ctx, "bl", "u")
	if err != nil || b.CompletedAt.IsZero() {
		t.Fatalf("failed mark never recovered: %+v, %v", b, err)
	}
	if ids, err := stores.Index.IDsOf(ctx, "bl", "u"); err != nil || len(ids) != 0 {
		t.Fatalf("personal data after recovery = %v, %v", ids, err)
	}
}
