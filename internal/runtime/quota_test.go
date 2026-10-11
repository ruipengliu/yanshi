package runtime

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/model/echo"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// TestQuotaSuspendsRechecksQuietlyAndResumes：超额时 Run 以配额原因挂起；复查时仍超额则只重新停放、不写事件；
// 配额撤销后在下一次复查时恢复并完成。
func TestQuotaSuspendsRechecksQuietlyAndResumes(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)) // 北京时间 10 点
	q := memqueue.New(clk)
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "echo", Version: "1", Model: "echo/any"})
	gw := model.NewGateway()
	gw.Register("echo", echo.Provider{})
	uses := usage.NewMem()
	limits := usage.Limits{EndUserDaily: 0.001}
	if err := limits.Prepare(); err != nil {
		t.Fatal(err)
	}
	quotas := &usage.Quotas{Store: uses, Limits: map[string]usage.Limits{"bl": limits}}
	w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw, Catalog: &capability.Catalog{Local: capability.NewRegistry()},
		Meter: &usage.Meter{Store: uses}, Quotas: quotas, QuotaRecheck: time.Minute})

	st := &session.State{SessionID: "s1"}
	created := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u",
		Agent: &v1.AgentRef{Name: "echo", Version: "1"}}}}
	err := store.Commit(ctx, st, created, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("hi")}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(ctx, "s1", workqueue.Class{})
	_ = uses.Record(ctx, &usage.Entry{ID: "u1", BusinessLine: "bl", EndUser: "u", Kind: usage.Model, Cost: usage.Yuan(0.001), At: clk.Now()})

	steps := func(n int) {
		for range n {
			if _, err := w.Step(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	load := func() *session.State {
		st, err := store.Load(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	steps(4)
	r := load().Run("r1")
	if r.Status != session.RunSuspended || r.SuspendReason != "end_user_quota" {
		t.Fatalf("run %s (reason %q), want suspended for the end user quota", r.Status, r.SuspendReason)
	}
	if want := time.Date(2026, 3, 1, 16, 0, 0, 0, time.UTC); !r.SuspendedUntil.Equal(want) {
		t.Fatalf("suspended until %s, want %s", r.SuspendedUntil, want)
	}
	seq := load().Seq

	// 复查间隔之前不会被认领；之后被认领，仍超额，重新停放且不写事件。
	step := func() bool {
		did, err := w.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return did
	}
	if step() {
		t.Fatal("claimed before the recheck interval")
	}
	clk.Advance(time.Minute)
	if !step() {
		t.Fatal("not claimed for the recheck")
	}
	if step() {
		t.Fatal("not parked again after the recheck")
	}
	if got := load().Seq; got != seq {
		t.Fatalf("recheck while over quota wrote %d events", got-seq)
	}

	quotas.Limits = nil // 调高配额
	clk.Advance(time.Minute)
	steps(6)
	if r := load().Run("r1"); r.Status != session.RunCompleted {
		t.Fatalf("run %s after quota was lifted, want completed", r.Status)
	}
}
