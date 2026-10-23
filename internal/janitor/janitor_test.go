package janitor_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
)

type env struct {
	core  *app.Core
	clk   *clock.Fake
	mem   *memory.MemStore
	stors app.Stores
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "day", Version: "1", Model: "echo/any", Captures: true},
		&agentdef.Def{Name: "project", Version: "1", Model: "echo/any"},
	)
	if err != nil {
		t.Fatal(err)
	}
	stores := app.Memory(clk)
	core := app.NewCore(stores, app.Options{Clock: clk, IDs: ids.Sequential, Agents: agents, Model: model.NewGateway(), Blobs: artifact.NewMemBlobs()})
	return &env{core: core, clk: clk, mem: stores.Memories.(*memory.MemStore), stors: stores}
}

func (e *env) session(t *testing.T, agent string) string {
	t.Helper()
	sid, err := e.core.Service.Create(context.Background(), service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func (e *env) remember(t *testing.T, sid string) {
	t.Helper()
	if _, err := e.core.Memory.Save(context.Background(), memory.SaveRequest{BusinessLine: "bl", EndUser: "u", SessionID: sid, CallID: "c_" + sid,
		Category: "preference", Content: "不吃香菜"}); err != nil {
		t.Fatal(err)
	}
}

// drain 推进 Janitor 直到清理队列为空。
func drain(t *testing.T, j *janitor.Janitor) {
	t.Helper()
	for range 100 {
		did, err := j.Step(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !did {
			return
		}
	}
	t.Fatal("janitor did not drain")
}

// 按保留期到期删除 Session 时保留它写入的 Memory；用户主动删除时一并删除（ADR-0037）。
func TestRetentionDeletionKeepsMemories(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	expired, removed := e.session(t, "day"), e.session(t, "day")
	e.remember(t, expired)
	e.remember(t, removed)
	if err := e.core.Service.Delete(ctx, expired, lifecycle.ReasonRetention, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.core.Service.Delete(ctx, removed, "end_user", ""); err != nil {
		t.Fatal(err)
	}
	drain(t, e.core.Janitor("j1", sandbox.NewFake(), nil, 0))
	for _, sid := range []string{expired, removed} {
		if tomb, err := e.stors.Deletions.Get(ctx, sid); err != nil || tomb.CompletedAt.IsZero() {
			t.Fatalf("deletion of %s not completed: %+v, %v", sid, tomb, err)
		}
	}
	if !e.mem.HasSession(expired) {
		t.Error("memories of a session deleted by retention were removed")
	}
	if e.mem.HasSession(removed) {
		t.Error("memories of a session the user deleted were kept")
	}
}

// 保留策略按 AgentDef 覆盖：旁听 Session 按覆盖关闭与删除（原因为 retention），其余 Session 按业务线的值。
func TestRetentionByAgent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	day, project := e.session(t, "day"), e.session(t, "project")
	retention := map[string]lifecycle.Retention{"bl": {
		CloseAfterIdle: 720 * time.Hour,
		Agents:         map[string]lifecycle.Retention{"day": {CloseAfterIdle: 36 * time.Hour, DeleteAfterClose: 168 * time.Hour}},
	}}
	j := e.core.Janitor("j1", sandbox.NewFake(), retention, 0)

	e.clk.Advance(37 * time.Hour)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	closed := func(sid string) bool {
		x, err := e.stors.Index.Get(ctx, sid)
		return err == nil && !x.ClosedAt.IsZero()
	}
	if !closed(day) || closed(project) {
		t.Fatalf("after 37h: day closed %v, project closed %v", closed(day), closed(project))
	}

	e.clk.Advance(169 * time.Hour)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	tomb, err := e.stors.Deletions.Get(ctx, day)
	if err != nil || tomb.Reason != lifecycle.ReasonRetention {
		t.Fatalf("day session tombstone = %+v, %v", tomb, err)
	}
	if gone, _ := lifecycle.Deleted(ctx, e.stors.Deletions, project); gone {
		t.Fatal("project session deleted by the capture retention")
	}
}

func TestRetentionRules(t *testing.T) {
	r := lifecycle.Retention{CloseAfterIdle: time.Hour, Agents: map[string]lifecycle.Retention{
		"b": {DeleteAfterClose: 2 * time.Hour}, "a": {CloseAfterIdle: 3 * time.Hour}}}
	rules := r.Rules()
	if len(rules) != 3 || rules[0].Agents.Only != "a" || rules[1].Agents.Only != "b" || !slices.Equal(rules[2].Agents.Except, []string{"a", "b"}) {
		t.Fatalf("rules = %+v", rules)
	}
	// 覆盖整条替换：b 没有 CloseAfterIdle，不沿用业务线的值。
	if rules[1].CloseAfterIdle != 0 || rules[2].CloseAfterIdle != time.Hour || rules[2].Agents.Match("a") || !rules[2].Agents.Match("c") {
		t.Fatalf("rules = %+v", rules)
	}
	if err := (lifecycle.Retention{Agents: map[string]lifecycle.Retention{"a": {Agents: map[string]lifecycle.Retention{"b": {}}}}}).Validate(); err == nil {
		t.Fatal("accepted a nested override")
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}
