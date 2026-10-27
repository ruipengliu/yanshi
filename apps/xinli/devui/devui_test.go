package devui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssetsAndConfig(t *testing.T) {
	h := Handler(Config{BusinessLines: []string{"bl"}, ConsoleURL: "http://localhost:8080/console"})
	for path, marker := range map[string]string{"/": "xinli · 旁听", "/app.js": "/v1/fixture/", "/xinli.js": "week_review", "/ui.js": "export function h", "/style.css": ".xinli-card"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 200 || !strings.Contains(r.Body.String(), marker) {
			t.Errorf("asset %s: %d", path, r.Code)
		}
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/config.json", nil))
	var config Config
	if err := json.Unmarshal(r.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.BusinessLines) != 1 || config.BusinessLines[0] != "bl" || config.ConsoleURL == "" {
		t.Fatalf("config: %+v", config)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/missing.js", nil))
	if r.Code != 404 {
		t.Fatalf("missing asset: %d", r.Code)
	}
}
