package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/model/openaicompat"
)

func TestLooptestCLIFlagsAndExecution(t *testing.T) {
	// 缺少 -url 时返回错误。
	if err := runLooptest(t.Context(), nil, io.Discard); err == nil {
		t.Fatal("expected error when -url is missing")
	}

	// 启动一个指向 CLI 托管假模型的极简循环服务：假模型监听地址由固定回环空闲端口给出。
	// 先找一个可用端口，让服务提前知道假模型的 base URL。
	var modelBase atomic.Value
	var gotAuth atomic.Value
	loopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		var req v1.LoopStepRequest
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &req); err != nil {
			http.Error(w, "bad req", http.StatusBadRequest)
			return
		}
		mreq := &model.Request{Model: "cli-test", System: req.GetSystem()}
		for _, m := range req.GetMessages() {
			mreq.Messages = append(mreq.Messages, model.Message{
				Role: model.Role(m.GetRole()), Content: m.GetContent(), ToolCalls: m.GetToolCalls(), ToolCallID: m.GetToolCallId(),
			})
		}
		for _, tool := range req.GetTools() {
			mreq.Tools = append(mreq.Tools, model.ToolSpec{
				Name: tool.GetName(), Description: tool.GetDescription(), InputSchema: json.RawMessage(tool.GetInputSchemaJson()),
			})
		}
		prov := &openaicompat.Provider{BaseURL: modelBase.Load().(string)}
		mresp, err := prov.Generate(r.Context(), mreq, nil)
		if err != nil {
			http.Error(w, "model error", http.StatusBadGateway)
			return
		}
		out, _ := (protojson.MarshalOptions{}).Marshal(&v1.LoopStepResponse{
			Content: mresp.Content, ToolCalls: mresp.ToolCalls,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(loopSrv.Close)

	// 通过带缓冲的 io.Writer 捕获启动时打印的假模型地址，在首个请求到来前喂给被测服务。
	// 这里用一个同步管道 writer：收到第一行假模型地址后立即设置 modelBase。
	ready := make(chan struct{})
	var buf bytes.Buffer
	w := &modelAddrSniffer{
		dst: &buf,
		onAddr: func(addr string) {
			modelBase.Store(addr)
			close(ready)
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 先跑一个会通过的子集（含 -header、-wait、-restart、-run）。
	err := runLooptest(ctx, []string{
		"-url", loopSrv.URL,
		"-model-addr", "127.0.0.1:0",
		"-header", "Authorization=Bearer test-tok",
		"-wait", "2s",
		"-restart", "true",
		"-run", "single-step,restart,streaming",
	}, w)
	if err != nil {
		t.Fatalf("runLooptest failed: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "被测服务须配置为调用它") || !strings.Contains(out, "PASS  single-step") || !strings.Contains(out, "PASS  restart") {
		t.Fatalf("unexpected CLI output:\n%s", out)
	}
	if v, _ := gotAuth.Load().(string); v != "Bearer test-tok" {
		t.Fatalf("Authorization header = %q, want Bearer test-tok", v)
	}

	// 验证失败时返回非 nil 错误（main 将以非零码退出）。
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(badSrv.Close)
	var failBuf bytes.Buffer
	if err := runLooptest(ctx, []string{
		"-url", badSrv.URL,
		"-model-addr", "127.0.0.1:0",
		"-run", "single-step",
	}, &failBuf); err == nil {
		t.Fatalf("expected non-nil error on failing scenario, output:\n%s", failBuf.String())
	}
	if !strings.Contains(failBuf.String(), "FAIL  single-step") {
		t.Fatalf("expected FAIL line, got:\n%s", failBuf.String())
	}
}

type modelAddrSniffer struct {
	dst    *bytes.Buffer
	onAddr func(string)
	done   bool
}

func (s *modelAddrSniffer) Write(p []byte) (int, error) {
	n, err := s.dst.Write(p)
	if !s.done {
		for _, line := range strings.Split(s.dst.String(), "\n") {
			if addr, ok := strings.CutPrefix(line, "假模型地址："); ok {
				s.done = true
				s.onAddr(strings.TrimSpace(addr))
				break
			}
		}
	}
	return n, err
}
