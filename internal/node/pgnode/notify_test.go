package pgnode

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
)

func TestInboxNotificationKeyIncludesEndUser(t *testing.T) {
	pool, notifier := pgtest.Fresh(t)
	in := NewInbox(pool, notifier)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !notifier.Ready() {
		select {
		case <-ctx.Done():
			t.Fatal("notifier did not start")
		case <-time.After(time.Millisecond):
		}
	}
	key := inboxKey("a:b", "node:12")
	other := inboxKey("a", "node:12")
	if key == other || inboxKey("a", "b:node:12") == key {
		t.Fatal("ambiguous inbox key")
	}
	own, stopOwn := notifier.Subscribe(pg.TopicInbox, key)
	defer stopOwn()
	foreign, stopForeign := notifier.Subscribe(pg.TopicInbox, other)
	defer stopForeign()
	// WaitFor 等待频道 LISTEN 生效，消费初始化重查；之后直接观察底层通知，不能靠重查版本掩盖串唤醒。
	if err := notifier.WaitFor(ctx, pg.TopicInbox, key, func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan struct{}{own, foreign} {
		select {
		case <-ch:
		default:
		}
	}
	if err := in.Put(ctx, "a", "node:12", &v1.Invoke{CallId: "c"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-foreign:
	case <-ctx.Done():
		t.Fatal("own notification not delivered")
	}
	select {
	case <-own:
		t.Fatal("notification crossed end users")
	default:
	}
	if err := in.Put(ctx, "a:b", "node:12", &v1.Invoke{CallId: "c"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-own:
	case <-ctx.Done():
		t.Fatal("numeric node suffix broke notification delivery")
	}
}
