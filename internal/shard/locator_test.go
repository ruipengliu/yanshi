package shard_test

import (
	"encoding/json"
	"strings"
	"testing"

	"yanshi/internal/shard"
)

func locationTable(t *testing.T) shard.Table {
	t.Helper()
	table, err := shard.NewTable(shard.Config{Shards: []int{0, 7}, Ranges: []shard.Range{{First: 0, Last: 0, Shard: 0}, {First: 1, Last: 1023, Shard: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func locator(t *testing.T, table shard.Table, fallback bool, entries ...shard.CompatibilityEntry) shard.Locator {
	t.Helper()
	c, err := shard.NewCompatibility(table, shard.CompatibilityConfig{Version: "migration-1", LegacyDataInShard0: fallback, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	l, err := shard.NewLocator(table, c)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLocatorTargets(t *testing.T) {
	table := locationTable(t)
	l := locator(t, table, false)
	for _, tc := range []struct {
		target shard.Target
		want   int
	}{
		{shard.Target{Kind: shard.SessionData, Key: "ses_v1_000_001_x"}, 0},
		{shard.Target{Kind: shard.SessionIndex, Key: "ses_v1_000_001_x"}, 7},
		{shard.Target{Kind: shard.Object, Key: "art_v1_001_x"}, 7},
		{shard.Target{Kind: shard.Object, Key: "mem_v1_001_x"}, 7},
		{shard.Target{Kind: shard.Object, Key: "grant_v1_001_x"}, 7},
		{shard.Target{Kind: shard.Object, Key: "use_v1_001_x"}, 7},
		{shard.Target{Kind: shard.Derived, Key: "call_v1_001_x"}, 7},
	} {
		got, err := l.Locate(tc.target)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %d, %v", tc.target, got, err)
		}
	}
	// EndUser 的字符串外形没有格式含义，包括看似损坏的新 ID。
	for _, user := range []string{"old-user", "ses_v99_bad"} {
		want, _ := table.Shard(shard.Hash(user), false)
		got, err := l.Locate(shard.Target{Kind: shard.EndUser, Key: user})
		if err != nil || got != want {
			t.Fatalf("EndUser: %d, %v", got, err)
		}
	}
	// 同槽并不相同，更不构成授权；定位器只保证查询地点相同。
	a, _ := l.Locate(shard.Target{Kind: shard.Object, Key: "art_v1_001_mine"})
	b, _ := l.Locate(shard.Target{Kind: shard.Object, Key: "art_v1_001_other"})
	if a != b {
		t.Fatal("same slot must select the same shard")
	}
}

func TestLocatorLegacyAndCollision(t *testing.T) {
	table := locationTable(t)
	for _, kind := range []shard.Kind{shard.SessionData, shard.SessionIndex, shard.Object, shard.Derived, shard.EndUser} {
		t.Run(string(kind), func(t *testing.T) {
			target := shard.Target{Kind: kind, Key: "legacy"}
			entry := shard.CompatibilityEntry{Target: target, Shard: 7}
			l := locator(t, table, true, entry)
			if got, err := l.Locate(target); err != nil || got != 7 {
				t.Fatalf("mapped: %d, %v", got, err)
			}
			unmapped := shard.Target{Kind: kind, Key: "unmapped"}
			want := 0
			if kind == shard.EndUser {
				want, _ = table.Shard(shard.Hash(unmapped.Key), false)
			}
			if got, err := l.Locate(unmapped); err != nil || got != want {
				t.Fatalf("unmapped: %d, %v", got, err)
			}
			l = locator(t, table, false, entry)
			_, err := l.Locate(unmapped)
			if kind != shard.EndUser && err == nil {
				t.Fatal("legacy fallback must be explicit")
			}
			if kind == shard.EndUser && err != nil {
				t.Fatal(err)
			}
		})
	}
	data := shard.CompatibilityEntry{Target: shard.Target{Kind: shard.SessionData, Key: "ses_old"}, Shard: 7}
	index := shard.CompatibilityEntry{Target: shard.Target{Kind: shard.SessionIndex, Key: "ses_old"}, Shard: 0}
	l := locator(t, table, false, data, index)
	if got, _ := l.Locate(data.Target); got != 7 {
		t.Fatal(got)
	}
	if got, _ := l.Locate(index.Target); got != 0 {
		t.Fatal(got)
	}
	for _, id := range []string{"mem_v1_001_old", "mem_v9_broken"} {
		e := shard.CompatibilityEntry{Target: shard.Target{Kind: shard.Object, Key: id}, Shard: 0, Historical: true}
		l := locator(t, table, false, e)
		if got, err := l.Locate(e.Target); err != nil || got != 0 {
			t.Fatalf("collision: %d, %v", got, err)
		}
	}
}

func TestLocatorRejectsDamagedIDs(t *testing.T) {
	l := shard.DefaultLocator()
	for _, kind := range []shard.Kind{shard.SessionData, shard.SessionIndex, shard.Object, shard.Derived} {
		for _, id := range []string{"", "ses_v2_000_000_x", "ses_v1_100_000_x", "ses_v1_000", "ses_v1_000_000_", "ses_v1_000_000_a/b", "ses_v01_000_000_x"} {
			if _, err := l.Locate(shard.Target{Kind: kind, Key: id}); err == nil {
				t.Errorf("accepted %s %q", kind, id)
			}
		}
	}
	for _, kind := range []shard.Kind{shard.SessionData, shard.SessionIndex} {
		if _, err := l.Locate(shard.Target{Kind: kind, Key: "art_v1_000_x"}); err == nil {
			t.Fatal("object used as Session")
		}
	}
	if _, err := (shard.Locator{}).Locate(shard.Target{Kind: shard.Object, Key: "old"}); err == nil {
		t.Fatal("zero locator")
	}
	if _, err := l.Locate(shard.Target{Kind: "unknown", Key: "old"}); err == nil {
		t.Fatal("unknown kind")
	}
}

func TestCompatibilityRejectsAmbiguousConfig(t *testing.T) {
	table := locationTable(t)
	for _, data := range []string{
		`{"version":"a","unknown":1}`,
		`{"version":"a","version":"b"}`,
		`{"version":"a","entries":[{"kind":"object","key":"x","key":"y","shard":0}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"x","shard":0},{"kind":"object","key":"x","shard":7}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"x","shard":1}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"x"}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"x","shard":null}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"x","shard":-1}]}`,
		`{"version":"a","entries":[{"kind":"wat","key":"x","shard":0}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"","shard":0}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"mem_v9_x","shard":0}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"mem_v1_000_x","shard":0}]}`,
		`{"version":"a","entries":[{"kind":"object","key":"x","shard":0,"extra":1}]}`,
		`{"entries":[]}`, `null`, `{"version":"a"} {}`, `{"version":"a"} garbage`,
	} {
		if _, err := shard.ParseCompatibility(table, []byte(data)); err == nil {
			t.Errorf("accepted %s", data)
		}
	}
}

func TestActivationAndImmutableIdentity(t *testing.T) {
	table := locationTable(t)
	c, _ := shard.NewCompatibility(table, shard.CompatibilityConfig{Version: "v", LegacyDataInShard0: true})
	if err := shard.ValidateActivation(table, c); err == nil {
		t.Fatal("unmapped historical EndUsers could split")
	}
	if _, err := shard.NewLocator(table, c); err == nil {
		t.Fatal("constructor bypassed activation")
	}
	c, _ = shard.NewCompatibility(table, shard.CompatibilityConfig{Version: "v"})
	if err := shard.ValidateActivation(table, c); err != nil {
		t.Fatal(err)
	}
	entries := []shard.CompatibilityEntry{{Target: shard.Target{Kind: shard.EndUser, Key: "user"}, Shard: 7}}
	l := locator(t, table, true, entries...)
	version, digest := l.Identity()
	entries[0].Shard = 0
	if got, _ := l.Locate(entries[0].Target); got != 7 {
		t.Fatal("caller mutated map")
	}
	if v, d := l.Identity(); v != version || d != digest || len(d) != 64 {
		t.Fatal("unstable identity")
	}
	other := locator(t, table, false, entries...)
	if _, d := other.Identity(); d == digest {
		t.Fatal("identity ignored configuration")
	}
	if _, err := shard.DefaultLocator().Locate(shard.Target{Kind: shard.Object, Key: "legacy"}); err != nil {
		t.Fatal(err)
	}
}

func TestParseDeployment(t *testing.T) {
	data := `{"table":{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":0}]},"compatibility":{"version":"one","legacy_data_in_shard0":true,"entries":[]}}`
	l, err := shard.ParseDeployment([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.Locate(shard.Target{Kind: shard.SessionData, Key: "old"}); err != nil || got != 0 {
		t.Fatal(got, err)
	}
	for _, bad := range []string{strings.Replace(data, `"first":0`, `"first":0,"first":1`, 1), strings.Replace(data, `"shards":[0]`, `"shards":[0],"bad":1`, 1), data + `{}`} {
		if _, err := shard.ParseDeployment([]byte(bad)); err == nil {
			t.Fatal("ambiguous deployment accepted")
		}
	}
	// 清单次序不影响摘要。
	entries := []shard.CompatibilityEntry{{Target: shard.Target{Kind: shard.Object, Key: "a"}, Shard: 0}, {Target: shard.Target{Kind: shard.Object, Key: "b"}, Shard: 7}}
	a := locator(t, locationTable(t), true, entries...)
	entries[0], entries[1] = entries[1], entries[0]
	b := locator(t, locationTable(t), true, entries...)
	_, da := a.Identity()
	_, db := b.Identity()
	if da != db {
		t.Fatal("entry ordering changed identity")
	}
	encoded, _ := json.Marshal(shard.CompatibilityConfig{Version: "v", Entries: entries})
	if _, err := shard.ParseCompatibility(locationTable(t), encoded); err != nil {
		t.Fatal(err)
	}
}

func TestActivationRejectsForeignTableAndEmptyConfig(t *testing.T) {
	table := locationTable(t)
	if err := shard.ValidateActivation(table, shard.Compatibility{}); err == nil {
		t.Fatal("zero compatibility map accepted")
	}
	c, err := shard.NewCompatibility(table, shard.CompatibilityConfig{Version: "v", Entries: []shard.CompatibilityEntry{{Target: shard.Target{Kind: shard.Object, Key: "old"}, Shard: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := shard.ValidateActivation(shard.Table{}, c); err == nil {
		t.Fatal("zero table accepted")
	}
	one, _ := shard.NewTable(shard.Config{Shards: []int{0}, Ranges: []shard.Range{{First: 0, Last: 1023, Shard: 0}}})
	if err := shard.ValidateActivation(one, c); err == nil {
		t.Fatal("compatibility map from another table accepted")
	}
}
