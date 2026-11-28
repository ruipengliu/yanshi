package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestServeLogFormat(t *testing.T) {
	var out bytes.Buffer
	logger, err := serveLogger(&out, "text")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("ready")
	if !strings.Contains(out.String(), "msg=ready") {
		t.Fatalf("text log = %q", &out)
	}

	out.Reset()
	logger, err = serveLogger(&out, "json")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("ready")
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil || record["msg"] != "ready" {
		t.Fatalf("JSON log = %q, error = %v", &out, err)
	}

	if err := serve([]string{"-log-format=yaml"}); err == nil || !strings.Contains(err.Error(), "-log-format") {
		t.Fatalf("serve returned %v for an unknown log format", err)
	}
}
