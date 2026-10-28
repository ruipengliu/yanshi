package janitor_test

import (
	"context"
	"slices"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/delegation"
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
		&agentdef.Def{Name: "day", Version: "1", Model: "echo/any"},
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

func TestAbsoluteDeadlineWithoutRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	deadline := e.clk.Now().Add(time.Hour)
	sid, err := e.core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "day", DeleteAt: deadline})
	if err != nil {
		t.Fatal(err)
	}
	e.remember(t, sid)
	j := e.core.Janitor("deadline", sandbox.NewFake(), nil, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if gone, _ := lifecycle.Deleted(ctx, e.stors.Deletions, sid); gone {
		t.Fatal("deleted before deadline")
	}
	e.clk.Advance(2 * time.Hour)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, j)
	tomb, err := e.stors.Deletions.Get(ctx, sid)
	if err != nil || tomb.Reason != lifecycle.ReasonRetention || tomb.CompletedAt.IsZero() {
		t.Fatalf("deadline tombstone: %+v, %v", tomb, err)
	}
	if !e.mem.HasSession(sid) {
		t.Fatal("deadline removed memory")
	}
}

func TestErasureAndDeadlineCascade(t *testing.T) {
	for _, reason := range []string{lifecycle.ReasonRetention, lifecycle.ReasonErasure} {
		t.Run(reason, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			deadline := e.clk.Now().Add(time.Hour)
			sid, err := e.core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "day", DeleteAt: deadline})
			if err != nil {
				t.Fatal(err)
			}
			run, err := e.core.Service.Submit(ctx, sid, model.TextBlocks("任务"))
			if err != nil {
				t.Fatal(err)
			}
			st, err := e.core.Service.Load(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			child := delegation.ChildSessionID(sid, "delegate")
			err = e.core.Service.Store.Commit(ctx, st,
				&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: run.RunID, Attempt: 1}}},
				&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: run.RunID, Attempt: 1, ToolCalls: []*v1.ToolCall{{CallId: "delegate", Capability: "agent__project", ArgumentsJson: `{"task":"任务"}`}}}}},
				&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: run.RunID, Attempt: 1, CallId: "delegate", NodeId: delegation.NodeID(child)}}},
			)
			if err != nil {
				t.Fatal(err)
			}
			d := &delegation.Delegator{Service: e.core.Service}
			if err := d.Dispatch(ctx, delegation.NodeID(child), &v1.Invoke{SessionId: sid, RunId: run.RunID, CallId: "delegate", Capability: "project", ArgumentsJson: `{"task":"任务"}`}); err != nil {
				t.Fatal(err)
			}
			cs, err := e.core.Service.Load(ctx, child)
			if err != nil || !cs.Created.GetDeleteAt().AsTime().Equal(deadline) {
				t.Fatalf("child deadline: %+v, %v", cs, err)
			}
			e.remember(t, child)
			if err := e.core.Service.Delete(ctx, sid, reason, ""); err != nil {
				t.Fatal(err)
			}
			drain(t, e.core.Janitor("cascade", sandbox.NewFake(), nil, 0))
			for _, id := range []string{sid, child} {
				tomb, err := e.stors.Deletions.Get(ctx, id)
				if err != nil || tomb.Reason != reason || tomb.CompletedAt.IsZero() {
					t.Fatalf("cascade: %+v, %v", tomb, err)
				}
			}
			if !e.mem.HasSession(child) {
				t.Fatal("child memory removed")
			}
			got, err := e.mem.ListBySource(ctx, "bl", "u", []string{sid, child}, e.clk.Now())
			if err != nil || len(got) != 1 || got[0].SourceSession != child {
				t.Fatalf("candidates after cleanup: %+v, %v", got, err)
			}
		})
	}
}

func TestRetentionCanPrecedeDeadline(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sid, err := e.core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "day", DeleteAt: e.clk.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.core.Service.Close(ctx, sid, "service", "done"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Hour)
	j := e.core.Janitor("earlier", sandbox.NewFake(), map[string]lifecycle.Retention{"bl": {DeleteAfterClose: time.Hour}}, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	drain(t, j)
	tomb, err := e.stors.Deletions.Get(ctx, sid)
	if err != nil || tomb.Reason != lifecycle.ReasonRetention || tomb.CompletedAt.IsZero() {
		t.Fatalf("retention: %+v, %v", tomb, err)
	}
}

func TestParentDeletionDuringDelegationKeepsReason(t *testing.T) {
	for _, reason := range []string{lifecycle.ReasonRetention, lifecycle.ReasonErasure} {
		t.Run(reason, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			sid := e.session(t, "day")
			child := delegation.ChildSessionID(sid, "racing")
			d := &delegation.Delegator{Service: e.core.Service, BeforeCreate: func(ctx context.Context, _ string) {
				if err := e.core.Service.Delete(ctx, sid, reason, ""); err != nil {
					t.Fatal(err)
				}
			}}
			if err := d.Dispatch(ctx, delegation.NodeID(child), &v1.Invoke{SessionId: sid, CallId: "racing", Capability: "project", ArgumentsJson: `{"task":"任务"}`}); err != nil {
				t.Fatal(err)
			}
			drain(t, e.core.Janitor("race", sandbox.NewFake(), nil, 0))
			tomb, err := e.stors.Deletions.Get(ctx, child)
			if err != nil || tomb.Reason != reason || tomb.CompletedAt.IsZero() {
				t.Fatalf("child race: %+v, %v", tomb, err)
			}
		})
	}
}
