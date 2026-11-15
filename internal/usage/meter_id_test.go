package usage_test

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/ids"
	"yanshi/internal/shard"
	"yanshi/internal/usage"
)

type capturedIDs struct {
	usage.Store
	entries []usage.Entry
}

func (s *capturedIDs) Record(ctx context.Context, e *usage.Entry) error {
	s.entries = append(s.entries, *e)
	return s.Store.Record(ctx, e)
}

func TestMeterIDsFollowRealEndUserAndPreserveLegacyKeys(t *testing.T) {
	ctx := context.Background()
	store := &capturedIDs{Store: usage.NewMem()}
	m := &usage.Meter{Store: store}
	sid, err := ids.SessionID("session", "user")
	if err != nil {
		t.Fatal(err)
	}
	scope := usage.Scope{BusinessLine: "bl", EndUser: "user", SessionID: sid}
	at := time.Unix(1000, 0)
	u := &v1.Usage{InputTokens: 1}
	m.Model(ctx, scope, "use_model", "agent@1", "model", u, at)
	m.Model(ctx, scope, "use_decision", "agent@1", "model", u, at)
	m.Model(ctx, scope, "use_compact", "agent@1", "model", u, at)
	m.Oneshot(ctx, scope, "use_oneshot", "brief@1", "model", u, at)
	m.Speech(ctx, scope, "speech_token", "agent@1", "model", 1, 0, at)
	m.Sandbox(ctx, scope, "sbx_external/调用", time.Second, at)
	m.Call(ctx, scope, "call_event/1", "agent@1", "model", usage.CallTokens{InputText: 1}, at)
	want := []string{"use_model", "use_decision", "use_compact", "use_oneshot", "speech_token", "sbx_external/调用", "call_event/1"}
	if len(store.entries) != len(want) {
		t.Fatalf("recorded %d entries", len(store.entries))
	}
	for i, e := range store.entries {
		if e.LegacyID != want[i] || e.DedupKey() != want[i] {
			t.Fatalf("lost legacy identity: %+v", e)
		}
		if slot, err := ids.SlotOf(e.ID); err != nil || slot != shard.Hash(scope.EndUser) {
			t.Fatalf("usage followed Session instead of EndUser: %s, %v", e.ID, err)
		}
	}
}
