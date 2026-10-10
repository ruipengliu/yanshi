package presence_test

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/presence"
	"yanshi/internal/presence/presencetest"
)

func TestMem(t *testing.T) {
	presencetest.Run(t, func(*testing.T) presence.Store { return presence.NewMem() })
}

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func TestViewersMergeByDevice(t *testing.T) {
	es := []*presence.Entry{
		{ConnID: "1", DeviceID: "mac", Label: "macbook", Expires: t0.Add(time.Minute)},
		{ConnID: "2", DeviceID: "mac", Label: "macbook", Focused: true, Expires: t0.Add(time.Minute)},
		{ConnID: "3", DeviceID: "ph", Label: "iphone", TypingUntil: t0.Add(5 * time.Second), Expires: t0.Add(30 * time.Second)},
		{ConnID: "4", DeviceID: "old", Label: "ipad", Expires: t0},
	}
	vs := presence.Viewers(es, t0)
	if len(vs) != 2 || vs[0].GetLabel() != "iphone" || !vs[0].GetTyping() || vs[1].GetDeviceId() != "mac" || !vs[1].GetFocused() {
		t.Fatalf("viewers = %v", vs)
	}
	if next := presence.NextChange(es, t0); !next.Equal(t0.Add(5 * time.Second)) {
		t.Fatalf("next change = %v, want typing expiry", next)
	}
	if vs := presence.Viewers(es, t0.Add(5*time.Second)); vs[0].GetTyping() {
		t.Fatal("typing should lapse after TypingUntil")
	}
}

// TestPutOnDeletedSessionIsWithdrawn：Session 已有删除记录时写入的在场记录被撤回（先写、后查）。
func TestPutOnDeletedSessionIsWithdrawn(t *testing.T) {
	ctx := context.Background()
	store, dels := presence.NewMem(), lifecycle.NewMemDeletions()
	svc := &presence.Service{Store: store, Deletions: dels, Clock: clock.NewFake(t0)}
	if _, err := dels.Mark(ctx, &lifecycle.Tombstone{SessionID: "s1", BusinessLine: "bl", RequestedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Put(ctx, &presence.Entry{SessionID: "s1", ConnID: "c", Expires: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if store.HasSession("s1") {
		t.Fatal("presence of a deleted session was kept")
	}
}
