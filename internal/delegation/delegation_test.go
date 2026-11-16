package delegation_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
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

func TestParseArgsArtifacts(t *testing.T) {
	const a, b = "art_v1_001_abc", "art_v1_002_def"
	got, err := delegation.ParseArgs(`{"task":"t","artifacts":["artifact://` + a + `"," ` + b + `","artifact://` + a + `"]}`)
	if err != nil || !slices.Equal(got.Artifacts, []string{a, b}) {
		t.Fatalf("artifacts = %v, %v", got.Artifacts, err)
	}
	for _, bad := range []string{
		`{"task":"t","artifacts":["https://example.com/x.png"]}`,
		`{"task":"t","artifacts":["art_legacy"]}`,
		`{"task":"t","artifacts":["artifact://"]}`,
	} {
		if _, err := delegation.ParseArgs(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	var many []string
	for i := range delegation.MaxArtifacts + 1 {
		many = append(many, fmt.Sprintf(`"art_v1_000_x%d"`, i))
	}
	if _, err := delegation.ParseArgs(`{"task":"t","artifacts":[` + strings.Join(many, ",") + `]}`); err == nil {
		t.Fatal("accepted more than MaxArtifacts")
	}
}

// 副本 ID 由目标 Session、委托调用与来源推导：重复派发、重复回写得到同一个 ID，落在目标 Session 的槽位上。
func TestCopyID(t *testing.T) {
	dst, err := ids.SessionID("child", "user")
	if err != nil {
		t.Fatal(err)
	}
	id, err := delegation.CopyID(dst, "call", "art_v1_000_src")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := delegation.CopyID(dst, "call", "art_v1_000_src")
	other, _ := delegation.CopyID(dst, "call2", "art_v1_000_src")
	if id != again || id == other {
		t.Fatalf("copy ids %s %s %s", id, again, other)
	}
	slot, err := ids.SlotOf(id)
	want, _ := ids.SessionSlotOf(dst)
	if err != nil || slot != want {
		t.Fatalf("copy %s slot %d, want %d (%v)", id, slot, want, err)
	}
}
