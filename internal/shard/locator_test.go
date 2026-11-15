package shard_test

import (
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

func locator(t *testing.T, table shard.Table, version string) shard.Locator {
	t.Helper()
	l, err := shard.NewLocator(table, version)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLocatorTargets(t *testing.T) {
	table := locationTable(t)
	l := locator(t, table, "v")
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
		{shard.Target{Kind: shard.Object, Key: "call_v1_001_x"}, 7},
	} {
		got, err := l.Locate(tc.target)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %d, %v", tc.target, got, err)
		}
	}
	// EndUser 的字符串外形没有格式含义，包括看似损坏的新 ID。
	for _, user := range []string{"old-user", "ses_v99_bad"} {
		want, _ := table.Shard(shard.Hash(user))
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

// 旧格式 ID 的兼容已在上线前删除（ADR-0051）：无版本 ID 在任何部署中都报错，不回退库 0。
func TestLocatorRejectsUnversionedAndDamagedIDs(t *testing.T) {
	for _, l := range []shard.Locator{shard.DefaultLocator(), locator(t, locationTable(t), "v")} {
		for _, kind := range []shard.Kind{shard.SessionData, shard.SessionIndex, shard.Object} {
			for _, id := range []string{"", "old", "s1", "ses_old", "art_x", "mem_call_1", "ses_v2_000_000_x", "ses_v1_100_000_x", "ses_v1_000", "ses_v1_000_000_", "ses_v1_000_000_a/b", "ses_v01_000_000_x"} {
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
		if _, err := l.Locate(shard.Target{Kind: "derived", Key: "call_v1_000_x"}); err == nil {
			t.Fatal("removed kind accepted")
		}
		if _, err := l.Locate(shard.Target{Kind: "unknown", Key: "art_v1_000_x"}); err == nil {
			t.Fatal("unknown kind")
		}
	}
	if _, err := (shard.Locator{}).Locate(shard.Target{Kind: shard.Object, Key: "art_v1_000_x"}); err == nil {
		t.Fatal("zero locator")
	}
}

func TestLocatorRequiresTableAndVersion(t *testing.T) {
	if _, err := shard.NewLocator(shard.Table{}, "v"); err == nil {
		t.Fatal("zero table accepted")
	}
	if _, err := shard.NewLocator(locationTable(t), ""); err == nil {
		t.Fatal("empty version accepted")
	}
}

func TestIdentity(t *testing.T) {
	table := locationTable(t)
	l := locator(t, table, "v")
	version, digest := l.Identity()
	if v, d := l.Identity(); v != version || d != digest || len(d) != 64 || version != "v" {
		t.Fatal("unstable identity")
	}
	if _, d := locator(t, table, "w").Identity(); d == digest {
		t.Fatal("identity ignored the version")
	}
	one, _ := shard.NewTable(shard.Config{Shards: []int{0, 7}, Ranges: []shard.Range{{First: 0, Last: 1023, Shard: 7}}})
	if _, d := locator(t, one, "v").Identity(); d == digest {
		t.Fatal("identity ignored the table")
	}
}

func TestParseDeployment(t *testing.T) {
	data := `{"version":"one","table":{"shards":[0],"ranges":[{"first":0,"last":1023,"shard":0}]}}`
	l, err := shard.ParseDeployment([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := l.Identity(); v != "one" {
		t.Fatal(v)
	}
	if got, err := l.Locate(shard.Target{Kind: shard.SessionData, Key: "ses_v1_000_000_x"}); err != nil || got != 0 {
		t.Fatal(got, err)
	}
	for _, bad := range []string{
		strings.Replace(data, `"first":0`, `"first":0,"first":1`, 1),
		strings.Replace(data, `"shards":[0]`, `"shards":[0],"bad":1`, 1),
		strings.Replace(data, `"version":"one",`, ``, 1),
		strings.Replace(data, `"version":"one"`, `"version":"one","version":"two"`, 1),
		// 旧的兼容清单不再是部署配置的一部分，残留的字段必须报错，不能被静默忽略。
		strings.Replace(data, `}}`, `},"compatibility":{"version":"x","legacy_data_in_shard0":true,"entries":[]}}`, 1),
		data + `{}`,
		`null`,
	} {
		if _, err := shard.ParseDeployment([]byte(bad)); err == nil {
			t.Errorf("ambiguous deployment accepted: %s", bad)
		}
	}
}
