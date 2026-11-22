package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yanshi/internal/auth"
)

func TestServeDefinitionAccessConfig(t *testing.T) {
	for _, tc := range []struct {
		name, mode, config, want string
	}{
		{"missing agents", "jwt", "", "agents.allow is required"},
		{"missing allow", "jwt", "agents: {}\n", "agents.allow is required"},
		{"empty allow", "jwt", "agents: {allow: []}\n", "unknown storage"},
		{"invalid agent glob", "jwt", "agents: {allow: ['[']}\n", "agents.allow: invalid glob"},
		{"invalid oneshot glob", "jwt", "agents: {allow: ['*']}\noneshots: {allow: ['[']}\n", "oneshots.allow: invalid glob"},
		{"development", "none", "", "unknown storage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// jwt 模式的网关缺少建会话幂等密钥即拒绝启动（ADR-0059），先给出密钥才能走到定义访问的校验。
			t.Setenv("YANSHI_SESSION_KEY_SECRET", strings.Repeat("k", 32))
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "bl.yaml"), []byte("name: bl\n"+tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			err := serve([]string{"-auth=" + tc.mode, "-businesslines=" + dir,
				"-agents=" + t.TempDir(), "-oneshots=" + t.TempDir(), "-storage=invalid"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestKeygenAllowsDevelopmentDefinitions(t *testing.T) {
	dir := t.TempDir()
	if err := keygen([]string{"-businesslines=" + dir, "-key-out=" + filepath.Join(dir, "dev.key")}); err != nil {
		t.Fatal(err)
	}
	lines, err := auth.LoadDir(dir)
	if err != nil || len(lines) != 1 {
		t.Fatalf("load generated business line: %v", err)
	}
	a, err := auth.NewDefinitionAccess(lines)
	if err != nil {
		t.Fatal(err)
	}
	if !a.AllowsAgent("demo", "assistant") || !a.AllowsOneshot("demo", "brief") {
		t.Fatal("generated business line cannot access development definitions")
	}
}
