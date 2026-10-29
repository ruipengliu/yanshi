package delegation_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"yanshi/internal/delegation"
	"yanshi/internal/ids"
	"yanshi/internal/shard"
)

func TestChildSessionIDFollowsParentFormat(t *testing.T) {
	parent, err := ids.SessionID("parent", "user")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"ses_legacy", parent} {
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
		slot, legacy, err := ids.SessionSlotOf(child)
		if err != nil {
			t.Fatal(err)
		}
		if p == "ses_legacy" {
			if !legacy || child != "ses_"+token {
				t.Fatalf("legacy retry changed identity: %s", child)
			}
			continue
		}
		if legacy || slot != shard.Hash("ses_"+token) || len(child) != 47 {
			t.Fatalf("child data slot/format: %s", child)
		}
		parentSlot, _, _ := ids.SessionSlotOf(p)
		if slot == parentSlot {
			t.Fatal("test vector must distinguish child and parent slots")
		}
		u, legacy, err := ids.EndUserSlotOfSession(child)
		if err != nil || legacy || u != shard.Hash("user") {
			t.Fatalf("child EndUser slot: %s, %v", child, err)
		}
	}
	if _, err := delegation.ChildSessionID("ses_v2_000_000_token", "c", "user"); err == nil {
		t.Fatal("invalid version accepted as legacy")
	}
}
