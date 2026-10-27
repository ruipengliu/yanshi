package agentdef

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStreamAckDeclaration(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    bool
		invalid bool
	}{
		{"", false, false}, {"true", true, false}, {"false", false, false}, {"invalid", false, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			dir := t.TempDir()
			body := "name: test\nversion: '1'\nmodel: test/model\nstreams:\n  - name: transcript\n"
			if tc.value != "" {
				body += "    ack: " + tc.value + "\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "test.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			reg, err := LoadDir(dir)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid ack accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			def, err := reg.Latest("test")
			if err != nil {
				t.Fatal(err)
			}
			stream, ok := def.Stream("transcript")
			if !ok || stream.Ack != tc.want {
				t.Fatalf("stream = %+v, want ack %v", stream, tc.want)
			}
		})
	}
}
