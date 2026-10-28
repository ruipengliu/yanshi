package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// readOnlyWorker 保留 Started 与执行之间的 Step 边界，供测试替换当时的能力声明。
func readOnlyWorker(t *testing.T, cat *capability.Catalog, name string, log eventlog.Log) (*Worker, *session.Store, *clock.Fake) {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	q := memqueue.New(clk)
	store := &session.Store{Log: log, IDs: ids.Sequential("id"), Clock: clk}
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any",
		Capabilities: []string{"probe", "device:*"}})
	if err != nil {
		t.Fatal(err)
	}
	gw := model.NewGateway()
	gw.Register("v", callOnce{tool: name, args: "{}"})
	w := New(Config{ID: "w1", Store: store, Queue: q, Agents: agents, Model: gw, Catalog: cat})
	st := &session.State{SessionID: "s1"}
	if err := store.Commit(ctx, st,
		&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{
			BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "v", Version: "1"},
		}}},
		&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("查询")}}},
	); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, "s1", workqueue.Class{}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		stepReadOnlyWorker(t, w)
		st, err = store.Load(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		if c := st.Run("r1").PendingCall(); c != nil && c.Started() {
			return w, store, clk
		}
	}
	t.Fatal("call did not start")
	return nil, nil, nil
}

func stepReadOnlyWorker(t *testing.T, w *Worker) {
	t.Helper()
	if _, err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func readOnlyProbe(readOnly bool, invoked *int) *capability.Func {
	return &capability.Func{
		S: capability.Spec{Name: "probe", ReadOnly: readOnly, Idempotent: true, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Fn: func(context.Context, capability.Invocation) ([]*v1.ContentBlock, error) {
			*invoked = *invoked + 1
			return model.TextBlocks("ok"), nil
		},
	}
}

func readOnlyEvents(t *testing.T, store *session.Store) ([]*v1.ToolCallStarted, *v1.ToolResult) {
	t.Helper()
	events, err := eventlog.ReadAll(context.Background(), store.Log, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var starts []*v1.ToolCallStarted
	var result *v1.ToolResult
	for _, e := range events {
		if s := e.GetToolCallStarted(); s != nil {
			starts = append(starts, s)
		}
		if r := e.GetToolResult(); r != nil {
			result = r
		}
	}
	return starts, result
}

func TestReadOnlyDeclarationBetweenSteps(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after bool
		risk          v1.Risk
		wantInvoked   int
	}{
		{"unchanged read only", true, true, v1.Risk_RISK_LOW, 1},
		{"downgraded", true, false, v1.Risk_RISK_LOW, 0},
		{"downgraded with approval", true, false, v1.Risk_RISK_HIGH, 0},
		{"upgraded", false, true, v1.Risk_RISK_LOW, 1},
		{"unknown", false, false, v1.Risk_RISK_LOW, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoked := 0
			cp := readOnlyProbe(tc.before, &invoked)
			w, store, _ := readOnlyWorker(t, &capability.Catalog{Local: capability.NewRegistry(cp)}, "probe", memlog.New())
			if invoked != 0 {
				t.Fatal("executed in the Started step")
			}
			cp.S.ReadOnly, cp.S.Risk = tc.after, tc.risk
			stepReadOnlyWorker(t, w)
			starts, res := readOnlyEvents(t, store)
			if len(starts) != 1 || starts[0].GetReadOnly() != tc.before || res == nil || invoked != tc.wantInvoked {
				t.Fatalf("starts=%v result=%v invoked=%d", starts, res, invoked)
			}
			if tc.wantInvoked == 0 {
				if !res.GetIsError() || model.Text(res.GetContent()) != readOnlyChanged {
					t.Fatalf("downgrade result = %v", res)
				}
			} else if res.GetIsError() || model.Text(res.GetContent()) != "ok" {
				t.Fatalf("result = %v", res)
			}
			st, err := store.Load(context.Background(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			if st.Run("r1").CallsReadOnly() != tc.before || st.Run("r1").Calls[0].Approval != nil {
				t.Fatal("evidence changed or approval was requested after Started")
			}
		})
	}
}

func TestReadOnlyRoutedStartedIsUnknown(t *testing.T) {
	invoked := 0
	dir := node.NewMemDirectory(clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	if _, _, err := dir.Register(context.Background(), node.Info{
		NodeID: "n1", Scope: node.Scope{BusinessLine: "bl", EndUser: "u"}, Label: "phone", Kind: "phone",
		Capabilities: []*v1.CapabilitySpec{{Name: "read_file", Idempotent: true, Risk: v1.Risk_RISK_LOW, InputSchemaJson: `{"type":"object"}`}},
	}); err != nil {
		t.Fatal(err)
	}
	_, store, _ := readOnlyWorker(t, &capability.Catalog{Local: capability.NewRegistry(readOnlyProbe(true, &invoked)), Nodes: dir}, "phone__read_file", memlog.New())
	starts, _ := readOnlyEvents(t, store)
	if len(starts) != 1 || starts[0].GetNodeId() != "n1" || starts[0].GetReadOnly() {
		t.Fatalf("routed Started = %v", starts)
	}
}

func TestReadOnlyTakeoverEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second bool
	}{
		{"all read only", true, true},
		{"first unknown", false, true},
		{"second unknown", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoked := 0
			cp := readOnlyProbe(tc.first, &invoked)
			w, store, clk := readOnlyWorker(t, &capability.Catalog{Local: capability.NewRegistry(cp)}, "probe", memlog.New())
			cp.S.ReadOnly = tc.second
			clk.Advance(w.cfg.LeaseTTL + time.Second)
			cfg := w.cfg
			cfg.ID = "w2"
			next := New(cfg)
			stepReadOnlyWorker(t, next) // 接管
			stepReadOnlyWorker(t, next) // 新 Attempt 的 Started
			if invoked != 0 {
				t.Fatal("takeover executed before recording its own evidence")
			}
			stepReadOnlyWorker(t, next)
			starts, res := readOnlyEvents(t, store)
			if len(starts) != 2 || starts[0].GetReadOnly() != tc.first || starts[1].GetReadOnly() != tc.second ||
				starts[1].GetAttempt() != 2 || invoked != 1 || res == nil || res.GetIsError() {
				t.Fatalf("starts=%v result=%v invoked=%d", starts, res, invoked)
			}
			st, err := store.Load(context.Background(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			if st.Run("r1").CallsReadOnly() != (tc.first && tc.second) {
				t.Fatal("takeover discarded earlier evidence")
			}
		})
	}
}

func TestReadOnlyChangeAfterExecutionKeepsResult(t *testing.T) {
	invoked := 0
	cp := readOnlyProbe(true, &invoked)
	log := &failingResults{Log: memlog.New(), n: 1}
	w, store, _ := readOnlyWorker(t, &capability.Catalog{Local: capability.NewRegistry(cp)}, "probe", log)
	if _, err := w.Step(context.Background()); err == nil {
		t.Fatal("expected result commit failure")
	}
	if invoked != 1 {
		t.Fatalf("invoked = %d", invoked)
	}
	cp.S.ReadOnly, cp.S.Risk = false, v1.Risk_RISK_HIGH
	stepReadOnlyWorker(t, w)
	starts, res := readOnlyEvents(t, store)
	if len(starts) != 1 || !starts[0].GetReadOnly() || invoked != 1 || res == nil || res.GetIsError() || model.Text(res.GetContent()) != "ok" {
		t.Fatalf("starts=%v result=%v invoked=%d", starts, res, invoked)
	}
}

// MCP 走 ResultInvoker 接缝，必须与普通 Invoke 在相同检查之后执行。
type readOnlyResultProbe struct{ *capability.Func }

func (c readOnlyResultProbe) InvokeResult(ctx context.Context, inv capability.Invocation) (capability.Result, error) {
	content, err := c.Func.Invoke(ctx, inv)
	return capability.Result{Content: content}, err
}

func TestReadOnlyDowngradeBeforeResultInvoker(t *testing.T) {
	invoked := 0
	cp := readOnlyProbe(true, &invoked)
	w, store, _ := readOnlyWorker(t, &capability.Catalog{Local: capability.NewRegistry(readOnlyResultProbe{cp})}, "probe", memlog.New())
	cp.S.ReadOnly = false
	stepReadOnlyWorker(t, w)
	starts, res := readOnlyEvents(t, store)
	if len(starts) != 1 || !starts[0].GetReadOnly() || invoked != 0 || res == nil || !res.GetIsError() || model.Text(res.GetContent()) != readOnlyChanged {
		t.Fatalf("starts=%v result=%v invoked=%d", starts, res, invoked)
	}
}
