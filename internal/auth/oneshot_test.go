package auth

import (
	"os"
	"path/filepath"
	"testing"
)

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
