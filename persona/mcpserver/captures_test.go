package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/fixture"
	"persona/store/memstore"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCapturesMCP(t *testing.T) {
	ctx := context.Background()
	books := memstore.New(captureClock)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.Handle("/mcp", Handler(books, Options{Now: func() time.Time { return now }}))
	mux.Handle("/v1/fixture/", fixture.Handler(books, fixture.Options{Now: func() time.Time { return now }}))
	hs := httptest.NewServer(mux)
	defer hs.Close()
	conn, err := mcp.NewClient(&mcp.Implementation{Name: "captures-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	found := false
	for tool, err := range conn.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "captures" {
			found = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("captures must be read-only and idempotent")
			}
			if tool.Description != capturesTool.Description {
				t.Fatal("description changed")
			}
			got, _ := json.Marshal(tool.InputSchema)
			var want any
			_ = json.Unmarshal(capturesTool.InputSchema.(json.RawMessage), &want)
			expected, _ := json.Marshal(want)
			if string(got) != string(expected) {
				t.Fatal("schema changed")
			}
		}
	}
	if !found {
		t.Fatal("captures missing")
	}
	post := func(op, body string) {
		t.Helper()
		res, err := hs.Client().Post(hs.URL+"/v1/fixture/bl/u/"+op, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(op, res.StatusCode)
		}
	}
	post("captures", `{"date":"2026-10-27","capture_id":"a","segments":[{"speaker":"wearer","text":"原文不得出现在 content","spoken_at":"2026-10-27T08:00:00Z","time_zone":"UTC"}]}`)
	post("bind_day", `{"date":"2026-10-27","session":"s"}`)
	opts := domain.PageOptions{Limit: 200, MaxChars: 6000, GapMS: 300000, MaxToolResult: 8000}
	call := func(sid string, options any) *mcp.CallToolResult {
		t.Helper()
		meta := mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": sid}
		if options != nil {
			meta["yanshi/page_options"] = options
		}
		res, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: "captures", Arguments: map[string]any{}, Meta: meta})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, tc := range []struct {
		sid     string
		options any
		message string
	}{
		{"s", nil, "缺少平台旁听分页参数"}, {"unbound", opts, "这个 Session 没有绑定业务日"}, {"s", map[string]int{"limit": 0}, "平台旁听分页参数无效"},
	} {
		res := call(tc.sid, tc.options)
		if !res.IsError || len(res.Meta) != 0 || len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != tc.message {
			t.Fatalf("unexpected business error: %+v", res)
		}
	}
	res := call("s", opts)
	if res.IsError || res.Content[0].(*mcp.TextContent).Text != "旁听页见结构化元数据" || res.Meta["yanshi/page"] == nil {
		t.Fatal("structured page missing")
	}
	raw, _ := json.Marshal(res.Meta["yanshi/page"])
	var page domain.StructuredPage
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Segments) != 1 || page.Segments[0].Ref != "1.0" {
		t.Fatal("wrong page")
	}
	// 存储事务提交后 gap 才算固定，且读取不应推进处理进度。
	if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
		if b.CaptureProgress("2026-10-27") != "0" {
			t.Fatal("read advanced progress")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	post("bind_day", `{"date":"2026-10-27","session":"new"}`)
	if res := call("s", opts); !res.IsError || res.Meta["yanshi/page"] != nil {
		t.Fatal("old session read raw text")
	}
	if res := call("new", opts); res.IsError {
		t.Fatal("new session cannot read")
	}
	now, _ = domain.CaptureExpiresAt("2026-10-27", "UTC")
	if res := call("new", opts); !res.IsError || res.Meta["yanshi/page"] != nil || res.Content[0].(*mcp.TextContent).Text != domain.ErrCapturesExpired.Error() {
		t.Fatal("expired MCP page was not rejected")
	}
	for _, name := range []string{"file_day", "file_changes"} {
		res, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}, Meta: mcp.Meta{
			"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "new",
		}})
		if err != nil || len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != domain.ErrCapturesExpired.Error() {
			t.Fatal(name, res, err)
		}
	}

	r := XinliRegistration()
	if !r.AttestsSpeakers || !r.CiteCaptures || r.Acks {
		t.Fatal("registration flags")
	}
}
