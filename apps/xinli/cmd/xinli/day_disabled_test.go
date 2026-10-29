//go:build !daytest

package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestProductionHasNoDayControl(t *testing.T) {
	for _, flag := range []string{"-test-mode", "-test-control-url=http://127.0.0.1:1"} {
		if _, err := parseConfig([]string{"serve", flag}, io.Discard); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatal("production accepted day control flag")
		}
	}
}

func TestProductionHasNoDaySender(t *testing.T) {
	err := run(context.Background(), []string{"day-sender"}, strings.NewReader("{}"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatal("production accepted day sender")
	}
}
