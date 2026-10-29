//go:build daytest

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDayControlRequiresExplicitIsolation(t *testing.T) {
	args := productionArgs(t, "postgres://localhost/persona", "postgres://localhost/xinli", "http://127.0.0.1:8080")
	t.Setenv("XINLI_DAY_CONTROL_TOKEN", strings.Repeat("x", 32))
	for _, extra := range [][]string{
		{"-test-control-url", "http://127.0.0.1:1"},
		{"-test-mode"},
		{"-test-mode", "-test-control-url", "http://example.com"},
		{"-test-mode", "-test-control-url", "http://127.0.0.1:1", "-addr", ":8081"},
	} {
		if _, err := parseConfig(append(append([]string{}, args...), extra...), io.Discard); err == nil {
			t.Fatal("unisolated control accepted")
		}
	}
	cfg, err := parseConfig(append(args, "-test-mode", "-test-control-url", "http://127.0.0.1:1"), io.Discard)
	if err != nil || cfg.test == nil {
		t.Fatal("isolated control rejected")
	}
}

func TestDayClockRejectsRewind(t *testing.T) {
	var offset atomic.Int64
	offset.Store(int64(2 * time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("clock request not authenticated")
		}
		_ = json.NewEncoder(w).Encode(map[string]int64{"offset": offset.Load()})
	}))
	defer server.Close()
	c := &dayControl{base: server.URL, token: "token", http: server.Client()}
	if c.Now().Before(time.Now().Add(time.Hour)) {
		t.Fatal("clock offset not applied")
	}
	offset.Store(int64(time.Hour))
	defer func() {
		if recover() == nil {
			t.Error("clock rewind accepted")
		}
	}()
	c.Now()
}

func TestDaySenderNotEnabledByServeFlags(t *testing.T) {
	handled, err := testCommand(context.Background(), []string{"serve"}, strings.NewReader(""), io.Discard)
	if handled || err != nil {
		t.Fatal("sender intercepted serve")
	}
}

func TestDayExplicitFlagsOverrideInheritedSwitches(t *testing.T) {
	t.Setenv("XINLI_AUTO_SCHEDULE", "false")
	t.Setenv("XINLI_EXIT_ON_STDIN_EOF", "true")
	args := productionArgs(t, "postgres://localhost/persona", "postgres://localhost/xinli", "http://127.0.0.1:8080")
	cfg, err := parseConfig(append(args, "-auto-schedule=true", "-exit-on-stdin-eof=false"), io.Discard)
	if err != nil || !cfg.autoSchedule || cfg.exitOnEOF {
		t.Fatal("explicit day flags did not override inherited lifecycle defaults")
	}
}

func TestDayDiagnosticsAuthenticatesAndReadsClockEveryTime(t *testing.T) {
	var offset atomic.Int64
	offset.Store(int64(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{"offset": offset.Load()})
	}))
	defer server.Close()
	c := &dayControl{base: server.URL, token: "token", http: server.Client(), role: "orchestrator", autoSchedule: true}
	handler := c.Diagnostics()
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", "/debug/day", nil))
	if unauthorized.Code != 401 || c.reads != 0 {
		t.Fatal("unauthenticated diagnostics reached the clock")
	}
	for i, duration := range []time.Duration{time.Hour, 2 * time.Hour} {
		offset.Store(int64(duration))
		req := httptest.NewRequest("GET", "/debug/day", nil)
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		var got struct {
			Now    time.Time `json:"now"`
			Offset int64     `json:"offset_ns"`
			First  int64     `json:"first_offset_ns"`
			Reads  uint64    `json:"clock_reads"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil {
			t.Fatal("diagnostics unavailable")
		}
		if got.Offset != int64(duration) || got.First != int64(time.Hour) || got.Reads != uint64(i+1) || got.Now.Before(time.Now().Add(duration-time.Second)) {
			t.Fatal("diagnostics used a startup-only clock offset")
		}
	}
}
