package runtime

import (
	"context"
	"sync"
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
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// blockingModel 的第一次调用报告开始，然后阻塞到 release 关闭（返回一次 clock_now 调用，Run 继续）或 ctx 取消。
type blockingModel struct {
	started, release chan struct{}
	once             sync.Once
}

func (m *blockingModel) Generate(ctx context.Context, _ *model.Request, _ func(model.Delta)) (*model.Response, error) {
	m.once.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "c1", Capability: "clock_now", ArgumentsJson: "{}"}}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type drainSetup struct {
	store *session.Store
	q     *memqueue.Queue
	m     *blockingModel
	w     *Worker
}

func newDrainSetup(t *testing.T, drain time.Duration) *drainSetup {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	d := &drainSetup{q: memqueue.New(clk), m: &blockingModel{started: make(chan struct{}), release: make(chan struct{})}}
	d.store = &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any", Capabilities: []string{"clock_now"}})
	gw := model.NewGateway()
	gw.Register("v", d.m)
	d.w = New(Config{ID: "w1", Store: d.store, Queue: d.q, Agents: agents, Model: gw,
		Catalog:      &capability.Catalog{Local: capability.NewRegistry(capability.ClockNow(clk))},
		DrainTimeout: drain, IdleWait: time.Millisecond})
	st := &session.State{SessionID: "s1"}
	if err := d.store.Commit(ctx, st,
		&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "v", Version: "1"}}}},
		&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("几点了")}}}); err != nil {
		t.Fatal(err)
	}
	_ = d.q.Enqueue(ctx, "s1", st.WorkClass())
	return d
}

// run 启动 Worker，等模型调用开始后发出停机信号；返回 Run 结束时的通道。
func (d *drainSetup) run(t *testing.T) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.w.Run(ctx); close(done) }()
	select {
	case <-d.m.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model call never started")
	}
	cancel()
	return done
}

// handedOff 检查 Run 以 handoff 挂起、租约已归还（立即可被其他 Worker 认领），并返回日志。
func (d *drainSetup) handedOff(t *testing.T) []*v1.Event {
	t.Helper()
	events, err := eventlog.ReadAll(context.Background(), d.store.Log, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1].GetRunSuspended()
	if last.GetReason() != session.SuspendHandoff {
		t.Fatalf("last event %v, want a handoff suspension", events[len(events)-1])
	}
	l, err := d.q.Claim(context.Background(), "w2", time.Minute, workqueue.Pool{})
	if err != nil || l.SessionID != "s1" {
		t.Fatalf("session not claimable right after the hand-off: %+v, %v", l, err)
	}
	return events
}

// TestShutdownFinishesStepThenHandsOff：停机信号到达时进行中的模型调用继续执行到写完日志，然后 Run 被移交，
// 租约立即归还；接手的 Worker 恢复它，不算接管（ADR-0029）。
func TestShutdownFinishesStepThenHandsOff(t *testing.T) {
	d := newDrainSetup(t, 5*time.Second)
	done := d.run(t)
	time.Sleep(20 * time.Millisecond) // 停机信号已到，模型调用仍在进行
	close(d.m.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	events := d.handedOff(t)
	found := false
	for _, e := range events {
		found = found || e.GetAssistantMessage() != nil
	}
	if !found {
		t.Fatal("the in-flight model call was not committed before stopping")
	}
	st, err := session.Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Active(); r == nil || r.Status != session.RunSuspended || r.QuotaSuspended() {
		t.Fatalf("run after hand-off: %+v", r)
	}
}

// TestShutdownCancelsStepAfterDrainTimeout：这一步超过 DrainTimeout 仍未完成时取消它，同样移交 Run。
func TestShutdownCancelsStepAfterDrainTimeout(t *testing.T) {
	d := newDrainSetup(t, 30*time.Millisecond)
	done := d.run(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after the drain timeout")
	}
	for _, e := range d.handedOff(t) {
		if e.GetAssistantMessage() != nil {
			t.Fatal("model call should have been cancelled")
		}
	}
}
