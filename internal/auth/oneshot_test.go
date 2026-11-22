package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOneshotAllowAndBusinessLineLimit(t *testing.T) {
	dir := t.TempDir()
	config := "name: bl\nagents: {allow: []}\noneshots: {allow: [brief-*], max_concurrency: 3}\n"
	if err := os.WriteFile(filepath.Join(dir, "bl.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := LoadDir(dir)
	if err != nil || len(lines) != 1 {
		t.Fatalf("load business line: %v", err)
	}
	if lines[0].Oneshots.MaxConcurrency != 3 {
		t.Fatal("limit lost")
	}
	access, err := NewDefinitionAccess(lines)
	if err != nil {
		t.Fatal(err)
	}
	if !access.AllowsOneshot("bl", "brief-live") || access.AllowsOneshot("bl", "other") ||
		access.AllowsOneshot("other", "brief-live") || access.AllowsAgent("bl", "assistant") {
		t.Fatal("unexpected definition access")
	}
	lines[0].Oneshots.Allow = nil
	access, err = NewDefinitionAccess(lines)
	if err != nil || access.AllowsOneshot("bl", "brief-live") {
		t.Fatalf("undeclared oneshot allowed: %v", err)
	}
}

func TestOneshotBusinessLineLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bl.yaml")
	for _, tc := range []struct {
		value string
		valid bool
	}{{"0", true}, {"3", true}, {"-1", false}} {
		if err := os.WriteFile(path, []byte("name: bl\noneshots: {max_concurrency: "+tc.value+"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		lines, err := LoadDir(dir)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.value, err)
		}
		if tc.value == "3" && lines[0].Oneshots.MaxConcurrency != 3 {
			t.Fatal("limit lost")
		}
	}
}
