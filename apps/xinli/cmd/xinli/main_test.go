package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

func TestDevReadinessFixtureMCPAndEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stdin, input := io.Pipe()
	defer stdin.Close()
	defer input.Close()
	output, stdout := io.Pipe()
	defer output.Close()
	defer stdout.Close()
	var logs bytes.Buffer
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"dev", "-business-lines", "bl,other", "-mcp-dir", dir, "-exit-on-stdin-eof"}, stdin, stdout, &logs)
		_ = stdout.Close()
	}()
	type received struct {
		raw map[string]json.RawMessage
		err error
	}
	announced := make(chan received, 1)
	go func() {
		var raw map[string]json.RawMessage
		err := json.NewDecoder(output).Decode(&raw)
		announced <- received{raw, err}
	}()
	var raw map[string]json.RawMessage
	select {
	case r := <-announced:
		if r.err != nil {
			t.Fatal(r.err)
		}
		raw = r.raw
	case <-ctx.Done():
		t.Fatal("readiness timed out")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var info ready
	if err := json.Unmarshal(encoded, &info); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 4 || info.MCPURL != info.FixtureURL+"/mcp" || info.PageURL != info.FixtureURL+"/" {
		t.Fatalf("readiness: %s", encoded)
	}
	var regs []map[string]any
	if err := json.Unmarshal(raw["registrations"], &regs); err != nil {
		t.Fatal(err)
	}
	source := mcpserver.XinliRegistration()
	if len(regs) != 2 {
		t.Fatal("registration count")
	}
	for i, bl := range []string{"bl", "other"} {
		r := regs[i]
		// 对照 internal/mcpcap/mcpcap.go Server 的 yaml 标签；不要直接序列化 Persona Registration。
		for _, key := range []string{"name", "business_line", "url", "timeout", "tools", "cite_captures", "acks"} {
			if _, ok := r[key]; !ok {
				t.Errorf("missing key: %s", key)
			}
		}
		if len(r) != 7 || r["name"] != source.Name || r["business_line"] != bl || r["url"] != info.MCPURL || r["timeout"] != "30s" || r["cite_captures"] != true || r["acks"] != true {
			t.Fatalf("registration: %+v", r)
		}
		if !reflect.DeepEqual(info.Registrations[i].Tools.NoApproval, source.NoApproval) {
			t.Fatal("no_approval mismatch")
		}
		data, err := os.ReadFile(filepath.Join(dir, "xinli-"+bl+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var fromYAML map[string]any
		if err := yaml.Unmarshal(data, &fromYAML); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r, fromYAML) {
			t.Fatalf("YAML differs: %+v", fromYAML)
		}
	}
	httpClient := &http.Client{Timeout: 3 * time.Second}
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, info.FixtureURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s: status=%d body=%s", path, resp.StatusCode, data)
		}
		return data
	}
	prefix := "/v1/fixture/bl/u"
	request("POST", prefix+"/seed", `{"Projects":[{"id":"p","name":"项目"}],"Commitments":[{"ID":"c","Kind":"mine","Content":"报价","Project":"p"}]}`, 200)
	var state domain.State
	if err := json.Unmarshal(request("GET", prefix+"/state", "", 200), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Projects) != 1 || state.Projects[0].ID != "p" {
		t.Fatalf("state: %+v", state)
	}
	request("POST", prefix+"/edit", `{"id":"c","status":"done"}`, 200)
	request("POST", prefix+"/resolve", `{"session":"missing","exchange":"x","project":"p"}`, 400)
	request("POST", prefix+"/resolve_merge", `{"id":"missing","accept":true}`, 400)
	request("POST", prefix+"/week_review", `{}`, 503)
	for _, path := range []string{"/", "/app.js", "/xinli.js", "/ui.js", "/style.css", "/config.json"} {
		request("GET", path, "", 200)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: info.MCPURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: "find_related", Arguments: map[string]string{"query": "项目"}, Meta: mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "day"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) == 0 {
		t.Fatalf("MCP result: %+v", result)
	}
	_ = conn.Close()
	// 不取消 ctx，仅关闭父进程 stdin，证明 EOF 本身会触发退出。
	_ = input.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v; logs: %s", err, logs.String())
		}
	case <-ctx.Done():
		t.Fatal("EOF did not stop process")
	}
	extra, err := io.ReadAll(output)
	if err != nil || len(extra) != 0 {
		t.Fatalf("stdout after readiness: %q %v", extra, err)
	}
}

func TestDevRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"dev"}, {"dev", "-business-lines", "../bad"}, {"dev", "-business-lines", "ok,"}, {"dev", "-business-lines", "ok", "-console-url", "javascript:alert(1)"}} {
		if err := run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// 就绪输出用有缓冲的通道收取，测试不依赖固定端口或启动睡眠。
type readyWriter struct{ line chan []byte }

func (w readyWriter) Write(p []byte) (int, error) {
	w.line <- append([]byte(nil), p...)
	return len(p), nil
}

func TestDevWeekReviewWithKernel(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing service token")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/sessions":
			if body["business_line"] != "bl" || body["end_user"] != "u" || body["agent"] != "xinli-week" {
				t.Errorf("create: %v", body)
			}
			_, _ = io.WriteString(w, `{"session_id":"week-session"}`)
		case "/v1/sessions/week-session/inputs":
			if body["text"] != "回顾本周" || !strings.HasPrefix(body["input_id"], "xinli_week_") {
				t.Errorf("input: %v", body)
			}
			w.WriteHeader(202)
		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()
	t.Setenv("XINLI_TEST_TOKEN", "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	writer := readyWriter{make(chan []byte, 1)}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"dev", "-business-lines", "bl", "-kernel-url", backend.URL, "-kernel-token-env", "XINLI_TEST_TOKEN"}, strings.NewReader(""), writer, io.Discard)
	}()
	var info ready
	select {
	case raw := <-writer.line:
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("readiness timeout")
	}
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, "POST", info.FixtureURL+"/v1/fixture/bl/u/week_review", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		err = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || body["session_id"] != "week-session" {
			t.Fatalf("week: %+v %v status=%d", body, err, resp.StatusCode)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("shutdown timeout")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"/v1/sessions", "/v1/sessions/week-session/inputs", "/v1/sessions/week-session/inputs"}) {
		t.Fatalf("kernel calls: %v", calls)
	}
}
