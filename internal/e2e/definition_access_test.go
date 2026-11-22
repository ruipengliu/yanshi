package e2e

import (
	"context"
	"errors"
	"slices"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/auth"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/delegation"
	"yanshi/internal/model"
	"yanshi/internal/model/script"
	"yanshi/internal/node"
	"yanshi/internal/oneshot"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

func definitionAccessOptions(t *testing.T, lines ...auth.BusinessLine) app.Options {
	t.Helper()
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "a-private", Version: "1", Model: "script/any"},
		&agentdef.Def{Name: "b-private", Version: "1", Model: "script/any"},
		&agentdef.Def{Name: "shared", Version: "1", Model: "script/any", Capabilities: []string{"agent:*"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	releases, err := agents.ParseReleases([]byte(`b-private: {stable: "1"}`))
	if err != nil {
		t.Fatal(err)
	}
	agents.SetReleases(releases)
	shot := func(name string) *oneshot.Def {
		return &oneshot.Def{Name: name, Version: "1", Model: "script/any", Instructions: "概括输入。",
			Input: oneshot.Input{MaxChars: 100}, MaxOutputTokens: 50, Concurrency: 1}
	}
	shots, err := oneshot.NewRegistry(shot("a-private"), shot("b-private"))
	if err != nil {
		t.Fatal(err)
	}
	gw := model.NewGateway()
	gw.Register("script", script.Provider{})
	opts := app.Options{Clock: clock.Real{}, Agents: agents, Oneshots: shots, Model: gw,
		Blobs: artifact.NewMemBlobs(), NodeAuth: node.InsecureDevAuth{},
		SessionKeySecret: []byte("e2e-definition-access-session-key-secret")}
	if len(lines) > 0 {
		opts.DefinitionAccess = newDefinitionAccess(t, lines...)
	}
	return opts
}

func newDefinitionAccess(t *testing.T, lines ...auth.BusinessLine) *auth.DefinitionAccess {
	t.Helper()
	access, err := auth.NewDefinitionAccess(lines)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

func TestBusinessLineDefinitionAccess(t *testing.T) {
	for _, restricted := range []bool{true, false} {
		name := "configured"
		if !restricted {
			name = "nil"
		}
		t.Run(name, func(t *testing.T) {
			var lines []auth.BusinessLine
			if restricted {
				lines = []auth.BusinessLine{
					{Name: "a", Agents: auth.AgentSettings{Allow: []string{"a-*", "shared"}}, Oneshots: auth.OneshotSettings{Allow: []string{"a-*"}}},
					{Name: "b", Agents: auth.AgentSettings{Allow: []string{"b-*", "shared"}}, Oneshots: auth.OneshotSettings{Allow: []string{"b-*"}}},
				}
			}
			opts := definitionAccessOptions(t, lines...)
			core := app.NewCore(app.Memory(opts.Clock), opts)
			for _, tc := range []struct{ line, own, foreign string }{
				{"a", "a-private", "b-private"},
				{"b", "b-private", "a-private"},
			} {
				t.Run(tc.line, func(t *testing.T) {
					testDefinitionEntrances(t, core, tc.line, tc.own, tc.foreign, restricted)
				})
			}
		})
	}
}

func testDefinitionEntrances(t *testing.T, core *app.Core, line, own, foreign string, restricted bool) {
	t.Helper()
	ctx := context.Background()
	for _, version := range []string{"", "1"} {
		for _, name := range []string{own, "shared", foreign} {
			req := service.CreateRequest{BusinessLine: line, EndUser: "u", Agent: name, AgentVersion: version}
			for _, key := range []string{"", "key-" + name + "-" + version} {
				req.IdempotencyKey = key
				sid, err := core.Service.Create(ctx, req)
				if name == foreign && restricted {
					if !errors.Is(err, service.ErrInvalid) || sid != "" {
						t.Fatalf("create %s@%s with key %q: %q, %v", name, version, key, sid, err)
					}
					want := "invalid argument: agent \"" + name + "\" not found"
					if version != "" {
						want = "invalid argument: agent " + name + "@" + version + " not found"
					}
					if err.Error() != want {
						t.Fatalf("create error = %v, want %q", err, want)
					}
				} else if err != nil || sid == "" {
					t.Fatalf("create %s@%s with key %q: %q, %v", name, version, key, sid, err)
				}
			}
		}
	}
	tools, err := core.Catalog.Tools(ctx, capability.Target{Scope: node.Scope{BusinessLine: line, EndUser: "u"}}, []string{"agent:*"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Spec.Name)
	}
	wantTools := []string{"agent__a-private", "agent__b-private", "agent__shared"}
	if restricted {
		wantTools = []string{"agent__" + own, "agent__shared"}
	}
	if !slices.Equal(names, wantTools) {
		t.Fatalf("delegate tools = %v, want %v", names, wantTools)
	}
	parent, err := core.Service.Create(ctx, service.CreateRequest{BusinessLine: line, EndUser: "u", Agent: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{own, "shared", foreign} {
		inv := &v1.Invoke{SessionId: parent, RunId: "run_parent", CallId: "call_" + name, Capability: name, ArgumentsJson: `{"task":"你好"}`}
		child, err := delegation.ChildSessionID(parent, inv.CallId, "u")
		if err != nil {
			t.Fatal(err)
		}
		if err := core.Delegator.Dispatch(ctx, delegation.NodeID(child), inv); err != nil {
			t.Fatal(err)
		}
		st, err := core.Store.Load(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		if name == foreign && restricted {
			if st.Created != nil {
				t.Fatalf("direct delegation created forbidden child %q", name)
			}
		} else if st.Created.GetBusinessLine() != line || st.Agent.GetName() != name || st.Delegated == "" {
			t.Fatalf("direct delegation did not submit to child %q: %+v", name, st.Created)
		}
	}
	for _, name := range []string{own, foreign, "missing"} {
		out, err := core.Oneshots.Call(ctx, oneshot.Request{BusinessLine: line, EndUser: "u", Name: name, Input: "你好"})
		if name == "missing" || name == foreign && restricted {
			if err != oneshot.ErrNotFound || out != nil {
				t.Fatalf("oneshot %q: %+v, %v", name, out, err)
			}
		} else if err != nil || out == nil || out.Name != name {
			t.Fatalf("oneshot %q: %+v, %v", name, out, err)
		}
	}
}

func TestWorkerContinuesSessionAfterDefinitionAccessRemoved(t *testing.T) {
	ctx := context.Background()
	line := auth.BusinessLine{Name: "a", Agents: auth.AgentSettings{Allow: []string{"a-private"}}}
	opts := definitionAccessOptions(t, line)
	stores := app.Memory(opts.Clock)
	core := app.NewCore(stores, opts)
	req := service.CreateRequest{BusinessLine: "a", EndUser: "u", Agent: "a-private", IdempotencyKey: "existing"}
	sid, err := core.Service.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	line.Agents.Allow = []string{}
	opts.DefinitionAccess = newDefinitionAccess(t, line)
	core = app.NewCore(stores, opts)
	if got, replay, err := core.Service.CreateWithReplay(ctx, req); err != nil || !replay || got != sid {
		t.Fatalf("existing session replay: %q, %t, %v", got, replay, err)
	}
	req.IdempotencyKey = "new"
	if _, err := core.Service.Create(ctx, req); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("new session after removal: %v", err)
	}
	run, err := core.Service.Submit(ctx, sid, model.TextBlocks("继续"))
	if err != nil {
		t.Fatal(err)
	}
	w := runtime.New(core.WorkerConfig("worker", workqueue.Pool{}, nil))
	for range 20 {
		if _, err := w.Step(ctx); err != nil {
			t.Fatal(err)
		}
		st, err := core.Store.Load(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		status := st.Run(run.RunID).Status
		if status == session.RunCompleted {
			return
		}
		if status.Terminal() {
			t.Fatalf("existing session ended with %s", status)
		}
	}
	t.Fatal("existing session did not complete its run")
}

func TestDelegationReplayAfterDefinitionAccessRemoved(t *testing.T) {
	ctx := context.Background()
	line := auth.BusinessLine{Name: "a", Agents: auth.AgentSettings{Allow: []string{"shared", "a-private"}}}
	opts := definitionAccessOptions(t, line)
	stores := app.Memory(opts.Clock)
	core := app.NewCore(stores, opts)
	parent, err := core.Service.Create(ctx, service.CreateRequest{BusinessLine: "a", EndUser: "u", Agent: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	inv := &v1.Invoke{SessionId: parent, RunId: "run_parent", CallId: "call_child", Capability: "a-private", ArgumentsJson: `{"task":"继续"}`}
	child, err := delegation.ChildSessionID(parent, inv.CallId, "u")
	if err != nil {
		t.Fatal(err)
	}
	// 创建子 Session 后、提交委托输入前重启，重投递必须继续完成同一次委托。
	_, err = core.Service.Create(ctx, service.CreateRequest{SessionID: child, BusinessLine: "a", EndUser: "u", Agent: "a-private",
		Parent: &v1.ParentRef{SessionId: parent, RunId: inv.RunId, CallId: inv.CallId, Ancestors: []*v1.AgentRef{{Name: "shared", Version: "1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	line.Agents.Allow = []string{"shared"}
	opts.DefinitionAccess = newDefinitionAccess(t, line)
	core = app.NewCore(stores, opts)
	if err := core.Delegator.Dispatch(ctx, delegation.NodeID(child), inv); err != nil {
		t.Fatal(err)
	}
	st, err := core.Store.Load(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if st.Delegated == "" {
		t.Fatal("existing child did not receive its delegated input")
	}
}
