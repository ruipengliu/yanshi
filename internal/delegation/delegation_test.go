package delegation_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"yanshi/internal/delegation"
	"yanshi/internal/ids"
	"yanshi/internal/shard"
)

func TestChildSessionID(t *testing.T) {
	parent, err := ids.SessionID("parent", "user")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{parent} {
		child, err := delegation.ChildSessionID(p, "external/call", "user")
		if err != nil {
			t.Fatal(err)
		}
		retry, err := delegation.ChildSessionID(p, "external/call", "user")
		if err != nil || retry != child {
			t.Fatalf("retry changed identity: %s, %s, %v", child, retry, err)
		}
		h := sha256.Sum256([]byte(p + "\x00external/call"))
		token := hex.EncodeToString(h[:16])
		slot, err := ids.SessionSlotOf(child)
		if err != nil {
			t.Fatal(err)
		}
		if slot != shard.Hash("ses_"+token) || len(child) != 47 {
			t.Fatalf("child data slot/format: %s", child)
		}
		parentSlot, _ := ids.SessionSlotOf(p)
		if slot == parentSlot {
			t.Fatal("test vector must distinguish child and parent slots")
		}
		u, err := ids.EndUserSlotOfSession(child)
		if err != nil || u != shard.Hash("user") {
			t.Fatalf("child EndUser slot: %s, %v", child, err)
		}
	}
	for _, bad := range []string{"ses_v2_000_000_token", "ses_legacy"} {
		if _, err := delegation.ChildSessionID(bad, "c", "user"); err == nil {
			t.Fatalf("invalid parent %q accepted", bad)
		}
	}
}
