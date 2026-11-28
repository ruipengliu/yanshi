package agentdef

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

func TestLoadDirs(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	write := func(dir, file, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, dir := range []string{first, second} {
		name := []string{"a", "b"}[i]
		write(dir, "first.yaml", fmt.Sprintf("name: %s\nversion: '2'\nmodel: echo/any\n", name))
		write(dir, "last.yaml", fmt.Sprintf("name: %s\nversion: '1'\nmodel: echo/any\n", name))
		write(dir, ReleasesFile, name+": {stable: '2'}\n")
	}
	for _, dirs := range [][]string{{first}, {first, second}} {
		r, files, err := LoadDirs(dirs)
		if err != nil {
			t.Fatal(err)
		}
		rel, _, err := files.Load()
		if err != nil {
			t.Fatal(err)
		}
		r.SetReleases(rel)
		if got := len(r.All()); got != 2*len(dirs) {
			t.Fatalf("loaded %d definitions from %v", got, dirs)
		}
		for _, name := range []string{"a", "b"}[:len(dirs)] {
			latest, err := r.Latest(name)
			if err != nil || latest.Version != "1" {
				t.Fatalf("latest %s = %v, %v", name, latest, err)
			}
			stable, err := r.Default(name)
			if err != nil || stable.Version != "2" {
				t.Fatalf("stable %s = %v, %v", name, stable, err)
			}
		}
	}
	write(first, ReleasesFile, "b: {stable: '2'}\n")
	_, files, err := LoadDirs([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.Load(); err == nil || !strings.Contains(err.Error(), filepath.Join(first, ReleasesFile)) {
		t.Fatalf("expected misplaced release to be rejected: %v", err)
	}
	write(second, "duplicate.yaml", "name: a\nversion: '3'\nmodel: echo/any\n")
	if _, _, err := LoadDirs([]string{first, second}); err == nil || !strings.Contains(err.Error(), "defined in both") {
		t.Fatalf("expected duplicate name to be rejected: %v", err)
	}
}
