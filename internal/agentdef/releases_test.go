package agentdef

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

func registry(t *testing.T) *Registry {
	t.Helper()
	var defs []*Def
	for _, v := range []string{"1", "2", "3"} {
		defs = append(defs, &Def{Name: "a", Version: v, Model: "echo/any"})
	}
	r, err := NewRegistry(defs...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func version(t *testing.T, r *Registry, eu string) string {
	t.Helper()
	d, err := r.Resolve("a", eu)
	if err != nil {
		t.Fatal(err)
	}
	return d.Version
}

func TestResolveWithoutReleasesUsesLatest(t *testing.T) {
	if v := version(t, registry(t), "u"); v != "3" {
		t.Fatalf("default version %s, want the last loaded (3)", v)
	}
}

func TestCanarySplitIsStableProportionalAndMonotonic(t *testing.T) {
	r := registry(t)
	set := func(pct float64) {
		rel, err := r.ParseReleases(fmt.Appendf(nil, "a:\n  stable: \"1\"\n  canary: {version: \"2\", percent: %v, end_users: [qa]}\n", pct))
		if err != nil {
			t.Fatal(err)
		}
		r.SetReleases(rel)
	}
	users := make([]string, 20000)
	for i := range users {
		users[i] = fmt.Sprintf("user-%d", i)
	}
	set(10)
	inCanary := map[string]bool{}
	for _, u := range users {
		if version(t, r, u) == "2" {
			inCanary[u] = true
		}
		if version(t, r, u) != version(t, r, u) {
			t.Fatalf("split for %s is not stable", u)
		}
	}
	if n := len(inCanary); n < 1800 || n > 2200 {
		t.Fatalf("%d of 20000 users in a 10%% canary", n)
	}
	if version(t, r, "qa") != "2" {
		t.Fatal("allowlisted end user not in canary")
	}
	set(30)
	for u := range inCanary {
		if version(t, r, u) != "2" {
			t.Fatalf("%s left the canary when the percentage grew", u)
		}
	}
	set(0)
	for _, u := range users[:1000] {
		if version(t, r, u) != "1" {
			t.Fatalf("%s in a 0%% canary", u)
		}
	}
}

func TestReleasesValidation(t *testing.T) {
	r := registry(t)
	for _, bad := range []string{
		"a: {stable: \"9\"}",
		"a: {canary: {version: \"2\", percent: 5}}",
		"a: {stable: \"1\", canary: {version: \"9\", percent: 5}}",
		"a: {stable: \"1\", canary: {version: \"2\", percent: 101}}",
		"a: {stable: \"1\", withdrawn: [\"1\"]}",
		"a: {stable: \"1\", canary: {version: \"2\", percent: 5}, withdrawn: [\"2\"]}",
		"b: {stable: \"1\"}",
	} {
		if _, err := r.ParseReleases([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestWithdrawn(t *testing.T) {
	r := registry(t)
	rel, err := r.ParseReleases([]byte("a: {stable: \"1\", withdrawn: [\"2\"]}"))
	if err != nil {
		t.Fatal(err)
	}
	r.SetReleases(rel)
	if to, ok := r.Withdrawn(&v1.AgentRef{Name: "a", Version: "2"}); !ok || to.Version != "1" {
		t.Fatalf("withdrawn 2 → %v %v, want stable 1", to, ok)
	}
	if _, ok := r.Withdrawn(&v1.AgentRef{Name: "a", Version: "3"}); ok {
		t.Fatal("3 reported withdrawn")
	}
}

// TestWatchReleasesKeepsPreviousOnInvalidFile：重新加载的内容校验失败时保留原配置；之后的合法内容生效。
func TestWatchReleasesKeepsPreviousOnInvalidFile(t *testing.T) {
	r := registry(t)
	path := filepath.Join(t.TempDir(), "releases.yaml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a: {stable: \"1\"}")
	rel, loaded, err := r.LoadReleases(path)
	if err != nil {
		t.Fatal(err)
	}
	r.SetReleases(rel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.WatchReleases(ctx, path, loaded, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	write("a: {stable: \"9\"}")
	time.Sleep(50 * time.Millisecond)
	if v := version(t, r, "u"); v != "1" {
		t.Fatalf("invalid reload applied: version %s", v)
	}
	write("a: {stable: \"2\"}")
	deadline := time.Now().Add(2 * time.Second)
	for version(t, r, "u") != "2" {
		if time.Now().After(deadline) {
			t.Fatal("valid reload not applied")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
