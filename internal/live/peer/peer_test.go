package peer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/session"
)

// countingBus 记录当前订阅数，用于观察远程拉取连接是否建立或断开。
type countingBus struct {
	*live.MemBus
	subs atomic.Int32
}

func (c *countingBus) Subscribe(sid string) (<-chan live.Delta, func()) {
	c.subs.Add(1)
	ch, cancel := c.MemBus.Subscribe(sid)
	return ch, func() { cancel(); c.subs.Add(-1) }
}

// process 是一个"进程"：本地 MemBus 与对外的内部增量接口。
type process struct {
	bus *countingBus
	srv *httptest.Server
}

func newProcess(t *testing.T, token string) *process {
	p := &process{bus: &countingBus{MemBus: live.NewMemBus()}}
	mux := http.NewServeMux()
	mux.Handle(Path, Handler{Local: p.bus, Token: token})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

type fixture struct {
	t     *testing.T
	store *session.Store
	st    *session.State
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, store: &session.Store{Log: memlog.New(), IDs: ids.Sequential("e"), Clock: clock.Real{}}}
	f.st = &session.State{SessionID: "s1"}
	f.commit(&v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}})
	f.commit(&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1"}}})
	return f
}

func (f *fixture) commit(e *v1.Event) {
	f.t.Helper()
	if err := f.store.Commit(context.Background(), f.st, e); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) attempt(n uint32, endpoint string) {
	f.commit(&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: "r1", Attempt: n, LiveEndpoint: endpoint}}})
}

// receive 持续在 from 上发布带 text 的增量，直到订阅方收到（远程连接是异步建立的）。
func receive(t *testing.T, from *live.MemBus, deltas <-chan live.Delta, text string) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case d := <-deltas:
			if d.Text == text {
				return
			}
		case <-tick.C:
			from.Publish(live.Delta{SessionID: "s1", RunID: "r1", Text: text})
		case <-deadline:
			t.Fatalf("delta %q never arrived", text)
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDeltasFollowTheExecutingProcess(t *testing.T) {
	f := newFixture(t)
	a, c := newProcess(t, "secret"), newProcess(t, "secret")
	local := live.NewMemBus()
	b := &Bus{Local: local, Log: f.store.Log, Self: "http://b.internal", Token: "secret", Retry: 20 * time.Millisecond}
	deltas, cancel := b.Subscribe("s1")
	defer cancel()

	// 本进程发布的增量照常送达。
	receive(t, local, deltas, "local")

	// Attempt 在进程 A 上执行：从 A 拉取。
	f.attempt(1, a.srv.URL)
	receive(t, a.bus.MemBus, deltas, "from a")

	// 被进程 C 接管：断开 A，改从 C 拉取。
	f.attempt(2, c.srv.URL)
	receive(t, c.bus.MemBus, deltas, "from c")
	eventually(t, "disconnect from a", func() bool { return a.bus.subs.Load() == 0 })

	// 挂起：不再拉取。
	f.commit(&v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: &v1.RunSuspended{RunId: "r1", Attempt: 2}}})
	eventually(t, "disconnect from c", func() bool { return c.bus.subs.Load() == 0 })

	// 恢复到本进程：只用本地增量，不建立远程连接。
	f.attempt(3, b.Self)
	receive(t, local, deltas, "local again")
	if a.bus.subs.Load() != 0 || c.bus.subs.Load() != 0 {
		t.Fatal("subscribed to a remote process for a local attempt")
	}

	cancel()
}

func TestHandlerRequiresToken(t *testing.T) {
	p := newProcess(t, "secret")
	resp, err := http.Get(p.srv.URL + Path + "s1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}
