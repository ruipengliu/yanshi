package channel

import (
	"context"
	"reflect"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/presence"
)

type touchedPresence struct {
	presence.Store
	sessions []string
}

func (s *touchedPresence) Touch(ctx context.Context, sid, conn string, expires time.Time) error {
	s.sessions = append(s.sessions, sid)
	return s.Store.Touch(ctx, sid, conn, expires)
}

func TestHeartbeatTouchesOnlyJoinedSubscriptions(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1000, 0))
	mem := presence.NewMem()
	store := &touchedPresence{Store: mem}
	c := &Conn{ctx: ctx, connID: "conn", h: &Handler{Clock: clk, Presence: &presence.Service{Store: store}},
		subs: map[string]*subscription{"s1": {joined: true}, "s2": {joined: true}, "unauthorized": {}}}
	for _, sid := range []string{"s1", "s2", "unauthorized", "unsubscribed"} {
		if err := mem.Put(ctx, &presence.Entry{SessionID: sid, ConnID: "conn", Expires: clk.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	c.touchPresence()
	if !reflect.DeepEqual(store.sessions, []string{"s1", "s2"}) {
		t.Fatalf("touched subscriptions: %v", store.sessions)
	}
	for _, sid := range []string{"s1", "s2", "unauthorized", "unsubscribed"} {
		es, _, err := mem.List(ctx, sid, clk.Now())
		want := 0
		if sid == "s1" || sid == "s2" {
			want = 1
		}
		if err != nil || len(es) != want {
			t.Fatalf("%s: entries %v, %v", sid, es, err)
		}
	}
}
