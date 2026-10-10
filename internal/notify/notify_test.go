package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/notify"
	"yanshi/internal/notify/notifytest"
	"yanshi/internal/presence"
)

func TestMemRegistry(t *testing.T) {
	notifytest.Run(t, func(*testing.T) notify.Registry { return notify.NewMemRegistry() })
}

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// fakePusher 记录推送；fail 中的设备推送失败（invalid 中的令牌无效）。
type fakePusher struct {
	fail, invalid map[string]bool
	pushed        []string
	msgs          []notify.Message
}

func (p *fakePusher) Push(_ context.Context, d *notify.Device, m notify.Message) error {
	switch {
	case p.invalid[d.DeviceID]:
		return notify.ErrInvalidToken
	case p.fail[d.DeviceID]:
		return errors.New("vendor unavailable")
	}
	p.pushed = append(p.pushed, d.DeviceID)
	p.msgs = append(p.msgs, m)
	return nil
}

func setup(t *testing.T) (*notify.Notifier, *fakePusher, *presence.Mem, *notify.MemRegistry) {
	ctx := context.Background()
	reg, pres, push := notify.NewMemRegistry(), presence.NewMem(), &fakePusher{fail: map[string]bool{}, invalid: map[string]bool{}}
	for i, id := range []string{"pc", "phone"} {
		if err := reg.Register(ctx, &notify.Device{BusinessLine: "bl", EndUser: "u", DeviceID: id, Platform: "apns", Token: "t-" + id},
			t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return &notify.Notifier{Registry: reg, Presence: pres, Pusher: push, Clock: clock.NewFake(t0.Add(time.Hour))}, push, pres, reg
}

var approval = notify.Notification{Kind: notify.Approval, BusinessLine: "bl", EndUser: "u", SessionID: "s1", RunID: "r1", CallID: "c1"}

// TestPushesMostRecentDevice：只推给最近使用的一台设备，消息只含种类与 ID。
func TestPushesMostRecentDevice(t *testing.T) {
	n, push, _, _ := setup(t)
	out, err := n.Deliver(context.Background(), approval)
	if err != nil || out != notify.Pushed || len(push.pushed) != 1 || push.pushed[0] != "phone" {
		t.Fatalf("outcome %s err %v pushed %v", out, err, push.pushed)
	}
	if m := push.msgs[0]; m != (notify.Message{Kind: notify.Approval, SessionID: "s1", RunID: "r1", CallID: "c1"}) {
		t.Fatalf("message = %+v", m)
	}
}

// TestNoPushWhileWatching：有设备正显示该 Session 时不推送；只是订阅（未聚焦）不算。
func TestNoPushWhileWatching(t *testing.T) {
	n, push, pres, _ := setup(t)
	ctx := context.Background()
	e := &presence.Entry{SessionID: "s1", ConnID: "c", DeviceID: "pc", Expires: t0.Add(2 * time.Hour)}
	_ = pres.Put(ctx, e)
	if out, _ := n.Deliver(ctx, approval); out != notify.Pushed {
		t.Fatalf("subscribed but not focused: %s", out)
	}
	e.Focused = true
	_ = pres.Put(ctx, e)
	if out, _ := n.Deliver(ctx, approval); out != notify.Watching || len(push.pushed) != 1 {
		t.Fatalf("focused: %s, pushed %v", out, push.pushed)
	}
	// 其他 Session 被聚焦不影响。
	other := approval
	other.SessionID = "s2"
	if out, _ := n.Deliver(ctx, other); out != notify.Pushed {
		t.Fatalf("another session focused: %s", out)
	}
}

// TestFallsBackAndDropsInvalidTokens：推送失败时退到下一台；令牌无效的设备撤销登记。
func TestFallsBackAndDropsInvalidTokens(t *testing.T) {
	n, push, _, reg := setup(t)
	ctx := context.Background()
	push.invalid["phone"] = true
	if out, _ := n.Deliver(ctx, approval); out != notify.Pushed || push.pushed[0] != "pc" {
		t.Fatalf("fallback: %v", push.pushed)
	}
	if ds, _ := reg.List(ctx, "bl", "u"); len(ds) != 1 || ds[0].DeviceID != "pc" {
		t.Fatalf("invalid token kept: %+v", ds)
	}
	push.fail["pc"] = true
	if out, _ := n.Deliver(ctx, approval); out != notify.PushFailed {
		t.Fatalf("all failing: %s", out)
	}
	if ds, _ := reg.List(ctx, "bl", "u"); len(ds) != 1 {
		t.Fatal("a temporarily failing device was unregistered")
	}
	_ = reg.DeleteEndUser(ctx, "bl", "u")
	if out, _ := n.Deliver(ctx, approval); out != notify.NoDevice {
		t.Fatalf("no devices: %s", out)
	}
}
