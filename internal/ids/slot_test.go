package ids

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"yanshi/internal/shard"
)

func TestSlotEncoding(t *testing.T) {
	for slot := 0; slot < shard.SlotCount; slot++ {
		encoded := encodeSlot(slot)
		if len(encoded) != 3 {
			t.Fatalf("slot %d: %q", slot, encoded)
		}
		id := "art_v1_" + encoded + "_token"
		got, err := SlotOf(id)
		if err != nil || got != slot {
			t.Fatalf("%s -> %d, %v", id, got, err)
		}
	}
	if encodeSlot(0) != "000" || encodeSlot(1023) != "0vv" {
		t.Fatal("slot wire format changed")
	}
}

// 旧格式（无版本）ID 的兼容已在上线前删除（ADR-0051）：它们与损坏的 ID 一样报错，不再回退槽位 0。
func TestUnversionedAndMalformedIDs(t *testing.T) {
	for _, id := range []string{"ses_0123456789abcdef0123456789abcdef", "ses_id-1", "art_art-1", "mem_call_1", "grant_x-1", "id-1", "session"} {
		for _, parse := range []func(string) (int, error){SlotOf, SessionSlotOf, EndUserSlotOfSession} {
			if slot, err := parse(id); err == nil {
				t.Fatalf("unversioned %q accepted -> %d", id, slot)
			}
		}
	}
	for _, id := range []string{"", "art_v1", "art_v2_000_x", "art_v01_000_x", "art_v1_00_x", "art_v1_0000_x", "art_v1_100_x", "art_v1_0VV_x", "art_v1_00w_x", "art_v1_000_", "art_v1_000_a/b", "art_v1_000_a b", "ses_v1_000_x", "ses_v1_000_100_x", "ses_v1_000_000_", "_v1_000_x"} {
		if _, err := SlotOf(id); err == nil {
			t.Errorf("invalid %q accepted", id)
		}
	}
	if _, err := SessionSlotOf("mem_v1_000_x"); err == nil {
		t.Fatal("an object ID was accepted as a new Session ID")
	}
	if _, err := EndUserSlotOfSession("art_v1_000_x"); err == nil {
		t.Fatal("an object ID was accepted as a Session index key")
	}
}

func TestObjectsFollowTheirKey(t *testing.T) {
	g := Sequential("x")
	sid, err := g.Session("u")
	if err != nil {
		t.Fatal(err)
	}
	if sid != "ses_v1_0t7_0e7_x-1" {
		t.Fatalf("Session wire format changed: %s", sid)
	}
	slot, err := SessionSlotOf(sid)
	if err != nil || slot != shard.Hash("ses_x-1") {
		t.Fatalf("Session = %s: %d, %v", sid, slot, err)
	}
	euSlot, err := EndUserSlotOfSession(sid)
	if err != nil || euSlot != shard.Hash("u") || euSlot == slot {
		t.Fatalf("index slot = %d, %v; Session slot = %d", euSlot, err, slot)
	}
	for _, prefix := range []string{"art", "call", "run"} {
		id, err := g.ForSession(prefix, sid)
		if err != nil {
			t.Fatal(err)
		}
		got, err := SlotOf(id)
		if err != nil || got != slot {
			t.Fatalf("%s follows token instead of Session: %d, %v", id, got, err)
		}
	}
	for _, prefix := range []string{"mem", "grant", "use"} {
		id, err := g.ForEndUser(prefix, "u")
		if err != nil {
			t.Fatal(err)
		}
		got, err := SlotOf(id)
		if err != nil || got != euSlot {
			t.Fatalf("%s follows Session instead of EndUser: %d, %v", id, got, err)
		}
	}
	if next := g(); next != "x-8" {
		t.Fatalf("generation consumed extra IDs: %s", next)
	}
	// EndUser 是外部给的真实键，即便字符串长得像新 ID，也不能读其中的槽位。
	external := "ses_v1_000_000_external"
	id, err := WithEndUser("mem", "call", external)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := SlotOf(id); err != nil || got != shard.Hash(external) {
		t.Fatalf("external EndUser was parsed as an ID: %d, %v", got, err)
	}
	// 无版本的 Session 不再派生旧格式对象。
	if id, err := Sequential("x").ForSession("art", "ses_old"); err == nil {
		t.Fatalf("unversioned Session produced %s", id)
	}
}

func TestDeterministicIDFormat(t *testing.T) {
	left, right := Sequential("sim"), Sequential("sim")
	for i := 0; i < 100; i++ {
		a, err := left.Session(fmt.Sprintf("user-%d", i))
		b, otherErr := right.Session(fmt.Sprintf("user-%d", i))
		if err != nil || otherErr != nil || a != b {
			t.Fatalf("generation diverged: %s, %s, %v, %v", a, b, err, otherErr)
		}
	}
	// 随机 token 仍保留全部 128 位；工件可经 ask_user、文件存储与 SDK 的现有字符限制。
	sid, err := Random().Session("用户")
	if err != nil {
		t.Fatal(err)
	}
	id, err := Random().ForSession("art", sid)
	if err != nil || len(id) != 43 || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(id) {
		t.Fatalf("artifact format = %s, %v", id, err)
	}
	if len(sid) != 47 || !regexp.MustCompile(`^ses_v1_[0-9a-v]{3}_[0-9a-v]{3}_[0-9a-f]{32}$`).MatchString(sid) {
		t.Fatalf("Session format = %s", sid)
	}
	a, err := SessionID(strings.Repeat("a", 32), "u")
	b, otherErr := SessionID(strings.Repeat("a", 32), "u")
	if err != nil || otherErr != nil || a != b {
		t.Fatalf("derived Session diverged: %s, %s, %v, %v", a, b, err, otherErr)
	}
}

func TestInvalidGeneration(t *testing.T) {
	for _, test := range []struct{ prefix, token, owner string }{
		{"ses", "x", "u"}, {"bad_prefix", "x", "u"}, {"", "x", "u"},
		{"art", "", "u"}, {"art", "a/b", "u"}, {"art", "x", ""},
	} {
		if _, err := WithEndUser(test.prefix, test.token, test.owner); err == nil {
			t.Errorf("accepted invalid EndUser object: %+v", test)
		}
		if _, err := WithSession(test.prefix, test.token, test.owner); err == nil {
			t.Errorf("accepted invalid Session object: %+v", test)
		}
	}
	if _, err := SessionID("", "u"); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := SessionID("x", ""); err == nil {
		t.Fatal("empty EndUser accepted")
	}
	if _, err := WithSession("art", "x", "ses_v1_100_000_x"); err == nil {
		t.Fatal("malformed Session accepted")
	}
}
