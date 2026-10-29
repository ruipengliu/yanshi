package sandbox_test

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/node"
	"yanshi/internal/sandbox"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// 沙箱只执行本 Session 的调用：工作区按 Node ID 打开，别的 Session 的调用即使进了它的 Inbox 也不执行。
func TestSandboxRunsOnlyItsOwnSessionsCalls(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	q := memqueue.New(clk)
	hub := &node.Hub{Dir: node.NewMemDirectory(clk), Inbox: node.NewMemInbox(), Clock: clk}
	hub.Store = &session.Store{Log: memlog.New(), Clock: clk, IDs: ids.Sequential("e")}
	st := &session.State{SessionID: "s_victim"}
	if err := hub.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{
		BusinessLine: "bl", EndUser: "victim", Agent: &v1.AgentRef{Name: "a", Version: "1"},
	}}}); err != nil {
		t.Fatal(err)
	}
	fake := sandbox.NewFake()
	r := &sandbox.Router{Hub: hub, Queue: q}
	victim := sandbox.NodeID("s_victim")
	inv := &v1.Invoke{SessionId: "s_mallory", RunId: "run_1", CallId: "call_1", Capability: "read_file", ArgumentsJson: `{"path":"notes.txt"}`}

	if err := r.Dispatch(ctx, "victim", victim, inv, workqueue.Class{}); err == nil {
		t.Fatal("router dispatched another session's call to a sandbox")
	}
	// 绕过 Router 直接放入（例如来自此前的数据）：控制器丢弃它，不创建、不读取沙箱。
	if err := hub.Inbox.Put(ctx, "victim", victim, inv); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, victim, workqueue.Class{}); err != nil {
		t.Fatal(err)
	}
	c := &sandbox.Controller{ID: "c1", Queue: q, Hub: hub, Provider: fake, Activity: sandbox.NewMemActivity(),
		Ledger: sandbox.NewMemLedger(), Clock: clk}
	if _, err := c.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.Exists(victim) {
		t.Fatal("sandbox created for another session's call")
	}
	if items, _, _ := hub.Inbox.Pending(ctx, "victim", victim); len(items) != 0 {
		t.Fatalf("call still pending: %v", items)
	}
}
