package session

import (
	"context"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func TestStoreDoesNotWritePendingScheduleSnapshot(t *testing.T) {
	snaps := NewMemSnapshots()
	store := &Store{Snapshots: snaps}
	st := &State{SessionID: "ses_v1_000_000_s", pendingSchedule: &v1.Event{}}
	if err := store.putSnapshot(context.Background(), st); err == nil {
		t.Fatal("pending Fired snapshot accepted")
	}
	if snaps.Has("ses_v1_000_000_s") {
		t.Fatal("pending Fired snapshot was persisted")
	}
}
