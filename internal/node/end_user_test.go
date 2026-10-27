package node_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/node"
	"yanshi/internal/notify"
)

func TestEndUserBarrierRejectsNodeAndPushRegistration(t *testing.T) {
	ctx := context.Background()
	c := clock.NewFake(time.Unix(1000, 0))
	d := lifecycle.NewMemDeletions()
	if _, err := d.MarkEndUser(ctx, &lifecycle.EndUserTombstone{BusinessLine: "bl", EndUser: "u", RequestID: "r", RequestedAt: c.Now()}); err != nil {
		t.Fatal(err)
	}
	h := &node.Hub{Dir: node.NewMemDirectory(c), Push: notify.NewMemRegistry(), Auth: node.InsecureDevAuth{}, Clock: c, Deletions: d}
	for _, clientOnly := range []bool{false, true} {
		_, err := h.Connect(ctx, &v1.Hello{NodeId: "n", BusinessLine: "bl", EndUser: "u", ClientOnly: clientOnly, PushToken: "token", PushPlatform: "apns"})
		if !errors.Is(err, node.ErrNotFound) {
			t.Fatalf("connect client_only=%v: %v", clientOnly, err)
		}
		if nodes, err := h.Dir.List(ctx, node.Scope{BusinessLine: "bl", EndUser: "u"}); err != nil || len(nodes) != 0 {
			t.Fatalf("nodes = %+v, %v", nodes, err)
		}
		if devices, err := h.Push.List(ctx, "bl", "u"); err != nil || len(devices) != 0 {
			t.Fatalf("push = %+v, %v", devices, err)
		}
	}
}
