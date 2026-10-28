package agentdef

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredCaptureConfiguration(t *testing.T) {
	if _, err := NewRegistry(&Def{Name: "a", Version: "1", Model: "echo/any", Capabilities: []string{"captures"}}); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired capability: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("name: a\nversion: '1'\nmodel: echo/any\nstreams: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "streams") {
		t.Fatalf("retired streams declaration: %v", err)
	}
}
