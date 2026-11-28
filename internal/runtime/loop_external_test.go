package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// scriptLoop 按顺序给出回答，记录收到的每一步；回答为 nil 或用完时返回错误。
type scriptLoop struct {
	steps   []LoopStep
	replies []*LoopReply
}

func (l *scriptLoop) Decide(_ context.Context, step LoopStep, _ func(model.Delta)) (*LoopReply, error) {
	l.steps = append(l.steps, step)
	if i := len(l.steps) - 1; i < len(l.replies) && l.replies[i] != nil {
		return l.replies[i], nil
	}
	return nil, errors.New("loop unavailable")
}

type loopsMap map[string]Loop

func (m loopsMap) Loop(businessLine, name string) (Loop, error) {
	if l, ok := m[businessLine+"/"+name]; ok {
		return l, nil
	}
	return nil, ErrLoopNotRegistered
}

type failingCap string

func (c failingCap) Spec() capability.Spec { return capability.Spec{Name: string(c)} }
func (failingCap) Invoke(context.Context, capability.Invocation) ([]*v1.ContentBlock, error) {
	return nil, errors.New("boom")
}

func reply(text string, state string, calls ...string) *LoopReply {
	r := &LoopReply{Response: &model.Response{Content: model.TextBlocks(text)}, State: []byte(state), StateVersion: "v1"}
	for i, c := range calls {
		r.ToolCalls = append(r.ToolCalls, &v1.ToolCall{CallId: "loop_" + string(rune('a'+i)), Capability: c, ArgumentsJson: `{}`})
	}
	return r
}

func externalWorker(t *testing.T, loops Loops) (*Worker, *session.Store, string) {
	t.Helper()
	ctx := context.Background()
	sid, _ := ids.SessionID("ext", "u")
	store := &session.Store{Log: memlog.New(), Clock: clock.Real{}, IDs: ids.Sequential("id")}
	q := memqueue.New(clock.Real{})
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Instructions: "指令", Loop: &agentdef.LoopRef{Server: "plan"},
		Capabilities: []string{"note", "fail"}, Context: agentdef.Context{Scope: agentdef.ScopeRun}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(ctx, &session.State{SessionID: sid},
		&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "a", Version: "1"}}}},
		&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r", Input: model.TextBlocks("work")}}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, sid, workqueue.Class{}); err != nil {
		t.Fatal(err)
	}
	w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: &recordingModel{}, Loops: loops,
		Catalog: &capability.Catalog{Local: capability.NewRegistry(namedCap("note"), failingCap("fail"))}})
	return w, store, sid
}

func runToEnd(t *testing.T, w *Worker, store *session.Store, sid string) []*v1.Event {
	t.Helper()
	ctx := context.Background()
	for range 30 {
		if _, err := w.Step(ctx); err != nil && !strings.Contains(err.Error(), "loop") {
			t.Fatal(err)
		}
		st, err := store.Load(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if st.Active() == nil {
			break
		}
	}
	events, err := eventlog.ReadAll(ctx, store.Log, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// 状态随回答写入日志、下一步带回；"上一步"按回答中的原顺序给出改写后的 ID 与结果，失败的结果以 IsError 标记
// （ADR-0062 第 2、5 条）。外部循环的 Run 不调用模型。
func TestExternalLoopCarriesStateAndPreviousStep(t *testing.T) {
	loop := &scriptLoop{replies: []*LoopReply{reply("先查", "s1", "note", "fail"), reply("好了", "s2")}}
	m := &recordingModel{}
	w, store, sid := externalWorker(t, loopsMap{"bl/plan": loop})
	tracer, recorder := tracing.NewRecorder()
	w.cfg.Tracer = tracer
	w.cfg.Model, w.cfg.Loop = m, ModelLoop{Model: m}
	events := runToEnd(t, w, store, sid)

	if len(loop.steps) != 2 || len(m.got) != 0 {
		t.Fatalf("loop steps %d, model calls %d", len(loop.steps), len(m.got))
	}
	first, second := loop.steps[0], loop.steps[1]
	if first.State != nil || first.Turn != 1 || first.RunID != "r" || first.SessionID != sid || first.EndUser != "u" ||
		first.Agent.GetName() != "a" || len(first.Previous) != 0 || first.Request.System != "指令" {
		t.Fatalf("first step: %+v", first)
	}
	if string(second.State) != "s1" || second.StateVersion != "v1" || second.Turn != 2 || len(second.Previous) != 2 {
		t.Fatalf("second step: state %q turn %d previous %d", second.State, second.Turn, len(second.Previous))
	}
	p0, p1 := second.Previous[0], second.Previous[1]
	if p0.Call.GetCapability() != "note" || p0.IsError || p1.Call.GetCapability() != "fail" || !p1.IsError ||
		strings.HasPrefix(p0.Call.GetCallId(), "loop_") {
		t.Fatalf("previous step: %+v %+v", p0, p1)
	}
	var states []string
	completed := false
	for _, e := range events {
		if am := e.GetAssistantMessage(); am != nil {
			states = append(states, string(am.GetLoopState()))
			if am.GetModel() != "loop/plan" {
				t.Fatalf("model %q", am.GetModel())
			}
		}
		completed = completed || e.GetRunCompleted() != nil
	}
	if strings.Join(states, ",") != "s1,s2" || !completed {
		t.Fatalf("states %v completed %v", states, completed)
	}
	var steps, calls []map[string]slog.Value
	var attempt string
	for _, rec := range recorder.Records() {
		f := map[string]slog.Value{}
		rec.Attrs(func(a slog.Attr) bool { f[a.Key] = a.Value; return true })
		switch f[tracing.FieldName].String() {
		case "model.call":
			t.Fatal("external loop recorded model.call")
		case "loop.step":
			steps = append(steps, f)
		case "tool.call":
			calls = append(calls, f)
		case "run.attempt":
			attempt = f[tracing.FieldSpanID].String()
		}
	}
	if len(steps) != 2 || len(calls) != 2 || attempt == "" {
		t.Fatalf("expected two steps and two calls under the Attempt, got %v, %v", steps, calls)
	}
	for i, f := range steps {
		if f[tracing.FieldTraceID].String() != tracing.TraceID("r") || f[tracing.FieldSession].String() != sid ||
			f[tracing.FieldRun].String() != "r" || f[tracing.FieldAttempt].Uint64() != 1 ||
			f[tracing.FieldTurn].Int64() != int64(i+1) || f[tracing.FieldOutcome].String() != "ok" ||
			f[tracing.FieldParentSpanID].String() != attempt {
			t.Errorf("step span = %v", f)
		}
	}
	for i, f := range calls {
		if f[tracing.FieldTraceID].String() != tracing.TraceID("r") || f[tracing.FieldSession].String() != sid ||
			f[tracing.FieldRun].String() != "r" || f[tracing.FieldAttempt].Uint64() != 1 ||
			f[tracing.FieldKind].String() != "local" || f[tracing.FieldCall].String() != second.Previous[i].Call.GetCallId() ||
			f[tracing.FieldOutcome].String() != []string{"ok", "error"}[i] || f[tracing.FieldParentSpanID].String() != attempt {
			t.Errorf("local call span = %v", f)
		}
	}
}

// 调用请求以外的工具、超限的状态、非文本的回复都不写日志，与模型错误同口径计数，达到上限时 Run 失败（ADR-0062 第 4 条）。
func TestExternalLoopInvalidRepliesAreNotLogged(t *testing.T) {
	big := reply("", "")
	big.State = bytes.Repeat([]byte("x"), session.MaxLoopState+1)
	media := reply("", "")
	media.Content = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: "image/png"}}}}
	loop := &scriptLoop{replies: []*LoopReply{reply("", "", "unknown_tool"), big, media}}
	w, store, sid := externalWorker(t, loopsMap{"bl/plan": loop})
	events := runToEnd(t, w, store, sid)
	if len(loop.steps) != 3 {
		t.Fatalf("loop asked %d times", len(loop.steps))
	}
	for _, e := range events {
		if e.GetAssistantMessage() != nil {
			t.Fatal("invalid reply logged")
		}
		if f := e.GetRunFailed(); f != nil {
			if !strings.Contains(f.GetReason(), "loop error") {
				t.Fatalf("reason %q", f.GetReason())
			}
			return
		}
	}
	t.Fatal("run did not fail")
}

// AgentDef 指向的外部循环在 Session 的业务线中没有登记：配置错误，Run 失败（ADR-0062 第 7 条）。
func TestExternalLoopNotRegistered(t *testing.T) {
	for name, loops := range map[string]Loops{"no registry": nil, "other line": loopsMap{"other/plan": &scriptLoop{}}} {
		t.Run(name, func(t *testing.T) {
			w, store, sid := externalWorker(t, loops)
			for _, e := range runToEnd(t, w, store, sid) {
				if f := e.GetRunFailed(); f != nil {
					if !strings.Contains(f.GetReason(), "not registered") {
						t.Fatalf("reason %q", f.GetReason())
					}
					return
				}
			}
			t.Fatal("run did not fail")
		})
	}
}

// 被拦截的回答不写状态（ADR-0062 第 5 条）。
func TestRefuseDropsLoopState(t *testing.T) {
	m := &v1.AssistantMessage{LoopState: []byte("s"), LoopStateVersion: "v", ToolCalls: calls("c")}
	refuse(m)
	if m.LoopState != nil || m.LoopStateVersion != "" || m.ToolCalls != nil {
		t.Fatalf("refused message keeps %+v", m)
	}
}

// 最近一条回答之后有结果与插话时，内置规则可以截在结果处，外部循环驱动的 Run 只能截在最近一条回答之前
// （ADR-0062 第 5 条，docs/formal-verification/external-loop 的 builtin_compaction 变体）。
func TestLoopCompactionKeepsLatestStep(t *testing.T) {
	big := model.TextBlocks(strings.Repeat("x", 600))
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("go")}).          // 2
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).                           // 3
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls("c1")}). // 4
		add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "c1", Content: big}).   // 5
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls("c2")}). // 6
		add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "c2", Content: big}).   // 7
		add(&v1.Steered{RunId: "r1", InputId: "in", Input: model.TextBlocks("还有")}) // 8
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agentdef.Context{Window: 1000, CompactAt: 0.75, KeepRecent: 0, MaxToolResult: 1000}
	req := &model.Request{Messages: Transcript(st, cfg.MaxToolResult)}
	if got, _ := planCompaction(st, req, cfg, true); got < 7 {
		t.Fatalf("builtin cut %d: the test no longer exercises a cut after the latest reply", got)
	}
	limit := loopCutLimit(st, "r1")
	if limit != 6 {
		t.Fatalf("limit %d", limit)
	}
	if got, _ := planCompactionBefore(st, req, cfg, true, limit); got == 0 || got >= 6 {
		t.Fatalf("loop cut %d, want in (0, 6)", got)
	}
	if prev := previousStep(st, "r1", 0); len(prev) != 1 || prev[0].Call.GetCallId() != "c2" || model.Text(prev[0].Content) != model.Text(big) {
		t.Fatalf("previous step %+v", prev)
	}
}

// 只看本轮、没有摘要模型的外部循环：超出窗口时 Run 失败，即使没有可用的截断点（第一步的输入就超窗），
// 也不把超窗的请求发给外部服务（ADR-0062 第 6 条）。
func TestExternalLoopWithoutSummaryModelFailsOnOverflow(t *testing.T) {
	loop := &scriptLoop{replies: []*LoopReply{reply("好", "")}}
	w, store, sid := externalWorker(t, loopsMap{"bl/plan": loop})
	def, err := w.cfg.Agents.Get(&v1.AgentRef{Name: "a", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	def.Context.Window = 100
	// 输入远超窗口：History 只有这一条输入，找不到截断点。
	ctx := context.Background()
	st, err := store.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: "r", InputId: "big",
		Input: model.TextBlocks(strings.Repeat("长", 2000))}}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range runToEnd(t, w, store, sid) {
		if f := e.GetRunFailed(); f != nil {
			if !strings.Contains(f.GetReason(), "summary_model") || len(loop.steps) != 0 {
				t.Fatalf("reason %q after %d loop steps", f.GetReason(), len(loop.steps))
			}
			return
		}
	}
	t.Fatal("run did not fail")
}

// Session 有了删除记录（清理尚未完成、日志还在）之后，Worker 不再把上下文视图发给外部循环（ADR-0062 第 7 条）：
// 第一步照常请求，标记删除后的第二步不发出。
func TestExternalLoopStopsAfterDeletion(t *testing.T) {
	ctx := context.Background()
	loop := &scriptLoop{replies: []*LoopReply{reply("先查", "s1", "note"), reply("好了", "s2")}}
	w, store, sid := externalWorker(t, loopsMap{"bl/plan": loop})
	deletions := lifecycle.NewMemDeletions()
	store.Deletions = deletions
	for range 10 {
		if _, err := w.Step(ctx); err != nil {
			t.Fatal(err)
		}
		if len(loop.steps) == 1 {
			break
		}
	}
	if len(loop.steps) != 1 {
		t.Fatalf("loop steps %d before deletion", len(loop.steps))
	}
	if _, err := deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: sid, BusinessLine: "bl", Reason: "end_user", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := w.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(loop.steps) != 1 {
		t.Fatalf("loop was asked %d times; want no step after the session was deleted", len(loop.steps))
	}
}
