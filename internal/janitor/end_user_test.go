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
	"yanshi/internal/usage"
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

type failingExpiryTiers struct {
	usage.TierStore
	fail bool
}

func (s *failingExpiryTiers) Delete(ctx context.Context, bl, eu string) error {
	if s.fail {
		return errors.New("tier cleanup unavailable")
	}
	return s.TierStore.Delete(ctx, bl, eu)
}

func TestExpiredEndUserBarrierClearsTierAfterWriterCrash(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	stores := app.Memory(clk)
	raw := stores.Tiers
	tiers := &failingExpiryTiers{TierStore: raw}
	stores.Tiers = tiers
	// 已撤掉配额配置也必须清除历史分配。
	core := app.NewCore(stores, app.Options{Clock: clk, EndUserGrace: time.Hour})
	if _, err := core.Service.DeleteEndUser(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	j := core.Janitor("j", nil, nil, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := stores.Deletions.GetEndUser(ctx, "bl", "u")
	if err != nil || b.CompletedAt.IsZero() {
		t.Fatalf("completed barrier: %+v, %v", b, err)
	}
	// 首次清理完成后在途写入才落盘，写入进程崩溃，没有执行复查。
	if err := raw.Put(ctx, "bl", "u", "pro", clk.Now()); err != nil {
		t.Fatal(err)
	}
	if tier, err := core.Service.Tiers.Get(ctx, "bl", "u"); err != nil || tier != "pro" {
		t.Fatalf("late assignment: %q, %v", tier, err)
	}
	clk.Advance(time.Hour - time.Nanosecond)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if tier, err := raw.Get(ctx, "bl", "u"); err != nil || tier != "pro" {
		t.Fatalf("cleanup ran before grace ended: %q, %v", tier, err)
	}
	clk.Advance(time.Nanosecond)
	tiers.fail = true
	if err := j.Sweep(ctx); err == nil {
		t.Fatal("cleanup failure was ignored")
	}
	if saved, err := stores.Deletions.GetEndUser(ctx, "bl", "u"); err != nil || saved.RequestID != b.RequestID || !saved.ExpiresAt.Equal(b.ExpiresAt) {
		t.Fatalf("cleanup failure removed or changed barrier: %+v, %v", saved, err)
	}
	if tier, err := raw.Get(ctx, "bl", "u"); err != nil || tier != "pro" {
		t.Fatalf("failed cleanup unexpectedly changed assignment: %q, %v", tier, err)
	}
	// 在失效前再填缓存，成功重试必须使它立即失效。
	if _, err := core.Service.Tiers.Get(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	tiers.fail = false
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Deletions.GetEndUser(ctx, "bl", "u"); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("retry retained barrier: %v", err)
	}
	for _, s := range []usage.TierStore{raw, core.Service.Tiers} {
		if tier, err := s.Get(ctx, "bl", "u"); err != nil || tier != "" {
			t.Fatalf("writer crash left tier after barrier removal: %q, %v", tier, err)
		}
	}
}
