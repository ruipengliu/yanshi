package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/runtime"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

type attestedSource struct{}

func (attestedSource) Tools(context.Context, string, []string) ([]capability.Tool, error) {
	t := attestedTool{}
	return []capability.Tool{{Spec: t.Spec(), Local: t}}, nil
}

type attestedTool struct{}

func (attestedTool) Spec() capability.Spec {
	return capability.Spec{Name: "mcp_pages__read", Idempotent: true, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t attestedTool) Invoke(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
	r, e := t.InvokeResult(ctx, inv)
	return r.Content, e
}

var attestedRaw = json.RawMessage(`{"kind":"page","span":"57.0","done":true,"segments":[{"ref":"57.0","speaker":"wearer","text":"原话"},{"ref":"57.1","speaker":"other","speaker_key":"other-1","text":"违规"}]}`)

func (attestedTool) InvokeResult(context.Context, capability.Invocation) (capability.Result, error) {
	return capability.Result{Page: attestedRaw, Content: model.TextBlocks("必须丢弃")}, nil
}

type pageModerator struct {
	mode  string
	calls []string
}

func (m *pageModerator) Check(_ context.Context, r moderation.Request) (moderation.Verdict, error) {
	if r.Stage != moderation.Input {
		return moderation.Verdict{}, nil
	}
	m.calls = append(m.calls, r.Text)
	if m.mode == "unavailable" {
		return moderation.Verdict{}, errors.New("不得泄漏的提供商详情")
	}
	return moderation.Verdict{Block: m.mode == "block" && strings.Contains(r.Text, "违规") || m.mode == "combined" && strings.Contains(r.Text, "\n")}, nil
}
func TestAttestedWorker(t *testing.T) {
	for _, mode := range []string{"allow", "block", "combined", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			clk := clock.NewFake(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))
			mod := &pageModerator{mode: mode}
			defs, err := agentdef.NewRegistry(&agentdef.Def{Name: "test", Version: "1", Model: "test/m", Capabilities: []string{"mcp:pages/*"}})
			if err != nil {
				t.Fatal(err)
			}
			gw := model.NewGateway()
			gw.Register("test", toolCallOnce{tool: "mcp_pages__read", args: `{}`})
			core := app.NewCore(app.Memory(clk), app.Options{Clock: clk, IDs: ids.Sequential, Agents: defs, Model: gw, MCP: attestedSource{}, Moderator: mod})
			st := &session.State{SessionID: "s"}
			if err := core.Store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "test", Version: "1"}}}}, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r", Input: model.TextBlocks("整理")}}}); err != nil {
				t.Fatal(err)
			}
			if err := core.Stores.Queue.Enqueue(ctx, "s", workqueue.Class{}); err != nil {
				t.Fatal(err)
			}
			cfg := core.WorkerConfig("w", workqueue.Pool{}, nil)
			store := *cfg.Store
			failures := &pageResultFailures{Log: store.Log, left: 2}
			store.Log = failures
			cfg.Store = &store
			w := runtime.New(cfg)
			for range 16 {
				if _, err := w.Step(ctx); err != nil && !errors.Is(err, errPageAppend) {
					t.Fatal(err)
				}
			}
			if failures.left != 0 {
				t.Fatal("append faults not exercised")
			}
			events, err := eventlog.ReadAll(ctx, core.Store.Log, "s", 0)
			if err != nil {
				t.Fatal(err)
			}
			var result *v1.ToolResult
			for _, e := range events {
				if e.GetToolResult() != nil {
					result = e.GetToolResult()
				}
			}
			if result == nil {
				t.Fatal("no result")
			}
			if mode == "unavailable" {
				if !result.GetIsError() || result.GetAttested() || model.Text(result.GetContent()) != "旁听页的内容安全检查暂不可用，稍后再读" {
					t.Fatal("outage not closed")
				}
				return
			}
			if !result.GetAttested() || result.GetIsError() || result.GetCaptureRead() != nil {
				t.Fatal("invalid attested result")
			}
			p, err := capture.ParsePage(attestedRaw, capture.PageOptions{Limit: 200, MaxChars: 6000})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "block" {
				p.Segments[1].Text = ""
				p.Segments[1].Redacted = true
			}
			if mode == "combined" {
				for _, g := range p.Segments {
					g.Text = ""
					g.Redacted = true
				}
			}
			want, _, err := capture.Render(p, 0)
			if err != nil {
				t.Fatal(err)
			}
			if model.Text(model.ExpandCaptures(result.GetContent())) != model.Text(model.ExpandCaptures(want)) {
				t.Fatal("unexpected rendered content")
			}
			calls := 1
			if mode != "allow" {
				calls = 3
			}
			if len(mod.calls) != calls || mod.calls[0] != "原话\n违规" {
				t.Fatalf("moderation calls = %v", mod.calls)
			}
		})
	}
}

var errPageAppend = errors.New("simulated result append failure")

type pageResultFailures struct {
	eventlog.Log
	left int
}

func (l *pageResultFailures) Append(ctx context.Context, sid string, expected uint64, events ...*v1.Event) (uint64, error) {
	for _, e := range events {
		if e.GetToolResult() != nil && l.left > 0 {
			l.left--
			return 0, errPageAppend
		}
	}
	return l.Log.Append(ctx, sid, expected, events...)
}
