package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"persona/store/memstore"
)

func TestFixtureClockConfiguration(t *testing.T) {
	t.Setenv("XINLI_KERNEL_URL", "")
	plain, err := parseConfig([]string{"dev", "-business-lines", "eval"}, io.Discard)
	if err != nil || plain.fixtureClock != nil {
		t.Fatalf("legacy fixture clock: %+v, %v", plain.fixtureClock, err)
	}
	for _, offset := range []string{"-240h", "0s", "48h"} {
		cfg, err := parseConfig([]string{"dev", "-business-lines", "eval", "-fixture-clock-offset", offset}, io.Discard)
		if err != nil || cfg.fixtureClock == nil {
			t.Fatalf("scenario clock %s: %v", offset, err)
		}
		d, _ := time.ParseDuration(offset)
		before := time.Now().Add(d)
		if now := cfg.now(); now.Before(before) || now.After(time.Now().Add(d)) {
			t.Fatalf("scenario offset %s: %v", offset, now)
		}
	}
	for _, args := range [][]string{
		{"serve", "-business-lines", "eval", "-fixture-clock-offset", "0s"},
		{"dev", "-business-lines", "eval", "-fixture-clock-offset", "bad"},
		{"dev", "-business-lines", "eval", "-fixture-clock-offset", "0s", "-kernel-url", "http://127.0.0.1:8080"},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid fixture clock: %v", args)
		}
	}
}

func TestFixtureClockOnlyAdvances(t *testing.T) {
	c := &fixtureClock{offset: -48 * time.Hour}
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"offset":"-24h"}`, http.StatusOK},
		{`{"offset":"-24h"}`, http.StatusOK},
		{`{"offset":"-48h"}`, http.StatusBadRequest},
		{`{}`, http.StatusBadRequest},
		{`{"offset":null}`, http.StatusBadRequest},
		{`{"offset":"bad"}`, http.StatusBadRequest},
		{`{"offset":"0s","unknown":true}`, http.StatusBadRequest},
		{`{"offset":"0s"} {}`, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		c.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/fixture/clock", strings.NewReader(tc.body)))
		if w.Code != tc.code {
			t.Fatalf("clock %s: HTTP %d, want %d", tc.body, w.Code, tc.code)
		}
	}
	if c.offset != -24*time.Hour {
		t.Fatal("invalid clock request changed the offset")
	}
}

func TestFixtureClockRouteOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := config{mode: "dev"}
		want := http.StatusNotFound
		if enabled {
			cfg.fixtureClock = &fixtureClock{}
			want = http.StatusOK
		}
		w := httptest.NewRecorder()
		routes(cfg, memstore.New(cfg.now), nil, nil).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/fixture/clock", strings.NewReader(`{"offset":"0s"}`)))
		if w.Code != want {
			t.Fatalf("clock route enabled=%v: HTTP %d, want %d", enabled, w.Code, want)
		}
	}
}
