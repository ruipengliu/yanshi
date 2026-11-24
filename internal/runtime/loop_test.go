package runtime

import (
	"context"
	"reflect"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// recordingModel 记录收到的请求，回复一条文本。
type recordingModel struct{ got []*model.Request }

func (m *recordingModel) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	m.got = append(m.got, req)
	return &model.Response{Content: model.TextBlocks("好")}, nil
}

// recordingLoop 记录收到的请求，回复一个带模型自拟 ID 的调用。
type recordingLoop struct{ got []*model.Request }

func (l *recordingLoop) Decide(_ context.Context, step LoopStep, _ func(model.Delta)) (*model.Response, error) {
	l.got = append(l.got, step.Request)
	return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "call_0", Capability: "note", ArgumentsJson: `{}`}}}, nil
}

func loopWorker(t *testing.T, m model.Provider, loop Loop) (*Worker, *session.Store, string) {
	t.Helper()
	ctx := context.Background()
	sid, _ := ids.SessionID("loop", "u")
	store := &session.Store{Log: memlog.New(), Clock: clock.Real{}, IDs: ids.Sequential("id")}
	q := memqueue.New(clock.Real{})
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "m/any", Instructions: "指令", Capabilities: []string{"note"}})
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
	// 不经 Gateway：它会去掉请求中模型引用的提供商前缀，两边的请求就无法直接比较。
	return New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: m, Loop: loop,
		Catalog: &capability.Catalog{Local: capability.NewRegistry(namedCap("note"))}}), store, sid
}

// stepUntil 推进 Worker 直到 done 成立（先开启 Attempt，再调用 Loop）。
func stepUntil(t *testing.T, w *Worker, done func() bool) {
	t.Helper()
	for range 8 {
		if _, err := w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if done() {
			return
		}
	}
	t.Fatal("loop never called")
}

// 替换 Loop 不改变平台组装的请求（前缀缓存依赖它逐字节不变），Loop 的回复照常经平台改写调用 ID 后写入日志，
// 模型本身不再被调用（ADR-0062 第 9 条）。
func TestLoopReceivesAssembledRequest(t *testing.T) {
	ctx := context.Background()
	builtin := &recordingModel{}
	w, _, _ := loopWorker(t, builtin, nil)
	stepUntil(t, w, func() bool { return len(builtin.got) > 0 })

	unused := &recordingModel{}
	loop := &recordingLoop{}
	w, store, sid := loopWorker(t, unused, loop)
	stepUntil(t, w, func() bool { return len(loop.got) > 0 })

	if len(builtin.got) != 1 || len(loop.got) != 1 {
		t.Fatalf("requests: model %d, loop %d", len(builtin.got), len(loop.got))
	}
	if !reflect.DeepEqual(builtin.got[0], loop.got[0]) {
		t.Fatalf("loop request differs from model request:\n%+v\n%+v", loop.got[0], builtin.got[0])
	}
	if len(unused.got) != 0 {
		t.Fatalf("model called %d times with a custom loop", len(unused.got))
	}
	events, err := eventlog.ReadAll(ctx, store.Log, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if m := e.GetAssistantMessage(); m != nil {
			if calls := m.GetToolCalls(); len(calls) != 1 || calls[0].GetCallId() == "call_0" {
				t.Fatalf("loop call ID not rewritten: %v", calls)
			}
			return
		}
	}
	t.Fatal("no assistant message")
}
