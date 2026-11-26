package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yanshi/internal/auth"
)

func TestIdentityDomainMapping(t *testing.T) {
	lines := []auth.BusinessLine{
		{Name: "a", IdentityDomain: "corp"},
		{Name: "b", IdentityDomain: "corp"},
		{Name: "solo"},
	}
	if fn := identityDomain("none", lines); fn != nil {
		t.Fatal("identityDomain in auth=none mode should be nil")
	}
	fn := identityDomain("jwt", lines)
	if fn == nil {
		t.Fatal("identityDomain in auth=jwt mode returned nil")
	}
	for _, tc := range []struct {
		bl         string
		wantDomain string
		wantKnown  bool
	}{
		{"a", "corp", true},
		{"b", "corp", true},
		{"solo", "", true},
		{"unknown", "", false},
	} {
		d, ok := fn(tc.bl)
		if d != tc.wantDomain || ok != tc.wantKnown {
			t.Errorf("identityDomain(%q) = (%q, %v), want (%q, %v)", tc.bl, d, ok, tc.wantDomain, tc.wantKnown)
		}
	}
}

func TestServeRejectsInvalidIdentityDomain(t *testing.T) {
	t.Setenv("YANSHI_SESSION_KEY_SECRET", strings.Repeat("k", 32))
	dir := t.TempDir()
	cfg := "name: bl\nidentity_domain: \" bad \"\nagents: {allow: ['*']}\n"
	if err := os.WriteFile(filepath.Join(dir, "bl.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := serve([]string{"-auth=jwt", "-businesslines=" + dir, "-agents=" + t.TempDir(), "-oneshots=" + t.TempDir(), "-storage=invalid"})
	if err == nil || !strings.Contains(err.Error(), "identity_domain") {
		t.Fatalf("serve err = %v, want identity_domain validation error", err)
	}
}
