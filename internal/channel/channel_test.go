package channel_test

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/channel"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/presence"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

type outbox struct {
	mu   sync.Mutex
	msgs []*v1.GatewayMessage
}

func (o *outbox) send(m *v1.GatewayMessage) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.msgs = append(o.msgs, m)
	return nil
}

func (o *outbox) find(pred func(*v1.GatewayMessage) bool) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, m := range o.msgs {
		if pred(m) {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// slowRemove 让撤下记录变慢（如数据库延迟），暴露旧订阅撤下与新订阅写入之间的竞争。
type slowRemove struct{ *presence.Mem }

func (s slowRemove) Remove(ctx context.Context, sid, conn string) error {
	time.Sleep(20 * time.Millisecond)
	return s.Mem.Remove(ctx, sid, conn)
}

func setup(t *testing.T) (*channel.Handler, *presence.Mem, string) {
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	store := &session.Store{Log: memlog.New(), IDs: ids.Random(), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents}
	sid, err := svc.Create(context.Background(), service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	mem := presence.NewMem()
	return &channel.Handler{Service: svc, Live: live.NewMemBus(), Presence: &presence.Service{Store: slowRemove{mem}, Clock: clock.Real{}}}, mem, sid
}

var me = channel.Identity{BusinessLine: "bl", EndUser: "u", DeviceID: "d1", Label: "phone", Kind: "mobile"}

func subscribe(sid string) *v1.NodeMessage {
	return &v1.NodeMessage{Msg: &v1.NodeMessage_Subscribe{Subscribe: &v1.Subscribe{SessionId: sid}}}
}

// TestResubscribeKeepsPresence：重复订阅时旧订阅撤下记录在前、新订阅写入在后，记录不会被旧订阅误删；
// 前台状态沿用。连接关闭后记录撤下。
func TestResubscribeKeepsPresence(t *testing.T) {
	h, mem, sid := setup(t)
	var out outbox
	c := h.Open(context.Background(), me, out.send)
	c.Handle(subscribe(sid))
	c.Handle(&v1.NodeMessage{Msg: &v1.NodeMessage_Activity{Activity: &v1.Activity{SessionId: sid, Focused: true}}})
	focused := func() bool {
		es, _, _ := mem.List(context.Background(), sid, time.Now())
		return len(es) == 1 && es[0].Focused && es[0].DeviceID == "d1"
	}
	// 每次都等前一个订阅写入记录之后再重复订阅：此时旧订阅退出时一定会撤下记录。
	for range 3 {
		eventually(t, "focused entry", focused)
		c.Handle(subscribe(sid))
	}
	eventually(t, "one focused entry after resubscribing", focused)
	time.Sleep(100 * time.Millisecond)
	if !focused() {
		t.Fatal("presence entry lost after resubscribing")
	}
	c.Close()
	if mem.HasSession(sid) {
		t.Fatal("presence entry left after the connection closed")
	}
}

// TestForeignSessionLeavesNoPresence：订阅他人的 Session 以 not_found 结束，不留下在场记录；
// 对未订阅的 Session 报告状态也不写入。
func TestForeignSessionLeavesNoPresence(t *testing.T) {
	h, mem, sid := setup(t)
	var out outbox
	other := me
	other.EndUser = "someone-else"
	c := h.Open(context.Background(), other, out.send)
	defer c.Close()
	c.Handle(&v1.NodeMessage{Msg: &v1.NodeMessage_Activity{Activity: &v1.Activity{SessionId: sid, Focused: true}}})
	c.Handle(subscribe(sid))
	c.Handle(&v1.NodeMessage{Msg: &v1.NodeMessage_Activity{Activity: &v1.Activity{SessionId: sid, Focused: true}}})
	eventually(t, "not_found", func() bool {
		return out.find(func(m *v1.GatewayMessage) bool { return m.GetSubscriptionEnded().GetReason() == "not_found" })
	})
	if mem.HasSession(sid) {
		t.Fatal("a foreign subscription wrote presence")
	}
}

func TestSubscriptionLimit(t *testing.T) {
	h, _, _ := setup(t)
	var out outbox
	c := h.Open(context.Background(), me, out.send)
	defer c.Close()
	// 用真实存在的 Session：不存在的会立即以 not_found 结束、让出名额。
	for range channel.MaxSubscriptions + 1 {
		sid, err := h.Service.Create(context.Background(), service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
		if err != nil {
			t.Fatal(err)
		}
		c.Handle(subscribe(sid))
	}
	eventually(t, "limit", func() bool {
		return out.find(func(m *v1.GatewayMessage) bool { return m.GetSubscriptionEnded().GetReason() == "limit" })
	})
}
