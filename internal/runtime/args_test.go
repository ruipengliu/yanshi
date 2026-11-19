package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// callOnce 的模型以给定参数请求一次 tool，拿到结果后结束。
type callOnce struct{ tool, args string }

func (m callOnce) Generate(_ context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			return &model.Response{Content: model.TextBlocks("好")}, nil
		}
	}
	return &model.Response{ToolCalls: []*v1.ToolCall{{CallId: "c1", Capability: m.tool, ArgumentsJson: m.args}}}, nil
}

// runCall 让模型以 args 调用一次 tool，返回最终投影、日志与本地能力的执行次数。
// 可用的工具：本地的 note（低风险）、本地的 send（需要审批）与设备 phone 上的 read_file。
func runCall(t *testing.T, tool, args string) (*session.State, []*v1.Event, int) {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	q := memqueue.New(clk)
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any",
		Capabilities: []string{"note", "send", "device:*"}})
	gw := model.NewGateway()
	gw.Register("v", callOnce{tool: tool, args: args})
	invoked := 0
	local := func(name string, risk v1.Risk) capability.Capability {
		return capability.Func{
			S: capability.Spec{Name: name, Risk: risk, InputSchema: json.RawMessage(`{"type":"object"}`)},
			Fn: func(context.Context, capability.Invocation) ([]*v1.ContentBlock, error) {
				invoked++
				return model.TextBlocks("ok"), nil
			},
		}
	}
	dir := node.NewMemDirectory(clk)
	if _, _, err := dir.Register(ctx, node.Info{NodeID: "n1", Scope: node.Scope{BusinessLine: "bl", EndUser: "u"},
		Label: "phone", Kind: "phone",
		Capabilities: []*v1.CapabilitySpec{{Name: "read_file", InputSchemaJson: `{"type":"object"}`}}}); err != nil {
		t.Fatal(err)
	}
	w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw,
		Catalog: &capability.Catalog{Local: capability.NewRegistry(local("note", v1.Risk_RISK_LOW), local("send", v1.Risk_RISK_HIGH)), Nodes: dir}})

	st := &session.State{SessionID: "ses_v1_000_000_s1"}
	created := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "v", Version: "1"}}}}
	if err := store.Commit(ctx, st, created, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: model.TextBlocks("做吧")}}}); err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(ctx, "ses_v1_000_000_s1", workqueue.Class{})
	for range 12 {
		_, _ = w.Step(ctx)
	}
	final, err := store.Load(ctx, "ses_v1_000_000_s1")
	if err != nil {
		t.Fatal(err)
	}
	events, err := eventlog.ReadAll(ctx, store.Log, "ses_v1_000_000_s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	return final, events, invoked
}

// 参数不是合法的 JSON 对象时，不审批、不派发、不执行：错误作为结果交还模型，由它改正。
func TestInvalidArgumentsReturnedToModel(t *testing.T) {
	bad := map[string]string{
		"extra brace": `{"date": "2026-03-01"}}`,
		"stray tag":   `{"habits": [], "suggestions" string="false">[]}`,
		"not object":  `["a"]`,
		"null":        `null`,
	}
	for _, tool := range []string{"note", "send", "phone__read_file"} {
		for name, args := range bad {
			t.Run(tool+"/"+name, func(t *testing.T) {
				final, events, invoked := runCall(t, tool, args)
				if r := final.Run("r1"); r.Status != session.RunCompleted {
					t.Fatalf("run %s, want completed", r.Status)
				}
				if invoked != 0 {
					t.Fatalf("capability executed %d times", invoked)
				}
				var res *v1.ToolResult
				for _, e := range events {
					switch {
					case e.GetApprovalRequested() != nil:
						t.Fatal("approval requested for invalid arguments")
					case e.GetToolCallStarted() != nil:
						t.Fatal("call started with invalid arguments")
					case e.GetToolResult() != nil:
						res = e.GetToolResult()
					}
				}
				if res == nil || !res.GetIsError() {
					t.Fatalf("result = %v, want an error", res)
				}
				if text := model.Text(res.GetContent()); !strings.Contains(text, "arguments are not a valid JSON object") {
					t.Fatalf("result text %q", text)
				}
			})
		}
	}
}

// 外部内容会触发 Memory 写入闸门：注定被 Check 拒绝的写入须先把原因交还模型，不能让用户批准一次无用的调用。
// 能通过 Check 的写入仍须审批，避免预检查意外绕过闸门。
func TestMemoryPrecheckBeforeApproval(t *testing.T) {
	cases := []struct {
		name, content string
		category      memory.Category
		healthAllowed bool
		wantError     string
	}{
		{"health disabled", "用户的母亲血压偏高", memory.Relationship, false, "not enabled"},
		{"id card", "身份证号 11010519491231002X", memory.Profile, false, "sensitive personal information"},
		{"valid relationship", "用户的母亲住在上海", memory.Relationship, false, ""},
		{"health enabled", "用户的母亲血压偏高", memory.Health, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
			mem := &memory.Service{Store: memory.NewMemStore(), Grants: memory.NewMemGrants(), Clock: clk,
				IDs: ids.Sequential("mem"), Health: func(_ context.Context, bl, _ string) (bool, error) { return bl == "bl" && tc.healthAllowed, nil }}
			checked := mem.Check(ctx, memory.SaveRequest{BusinessLine: "bl", EndUser: "u", SessionID: "ses_v1_000_000_s1", CallID: "c1",
				Category: tc.category, Content: tc.content})
			if tc.wantError == "" {
				if checked != nil {
					t.Fatalf("Check rejected valid content: %v", checked)
				}
			} else if !errors.Is(checked, memory.ErrRejected) || !strings.Contains(checked.Error(), tc.wantError) {
				t.Fatalf("Check = %v, want rejection containing %q", checked, tc.wantError)
			}
			args, err := json.Marshal(map[string]string{"category": string(tc.category), "content": tc.content})
			if err != nil {
				t.Fatal(err)
			}
			q := memqueue.New(clk)
			store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any",
				Capabilities: []string{"memory_save"}})
			if err != nil {
				t.Fatal(err)
			}
			gw := model.NewGateway()
			gw.Register("v", callOnce{tool: "memory_save", args: string(args)})
			w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw,
				Catalog: &capability.Catalog{Local: capability.NewRegistry(memory.Capabilities(mem)...)}})

			// 待记住的内容只出现在界面上，不在用户原话中；否则即使 Check 被放到闸门之后，这个用例也可能通过。
			ui := &v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{Content: tc.content}}}
			b := (&logBuilder{}).
				add(&v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "v", Version: "1"}}).
				add(&v1.RunRequested{RunId: "r1", Input: append(model.TextBlocks("看看这条资料"), ui)})
			st := &session.State{SessionID: "ses_v1_000_000_s1"}
			if err := store.Commit(ctx, st, b.events...); err != nil {
				t.Fatal(err)
			}
			if _, gated := memoryGate(st, &v1.ToolCall{Capability: "memory_save", ArgumentsJson: string(args)}); !gated {
				t.Fatal("test content did not trigger the memory gate")
			}
			if err := q.Enqueue(ctx, "ses_v1_000_000_s1", workqueue.Class{}); err != nil {
				t.Fatal(err)
			}
			for range 12 {
				if _, err := w.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			final, err := store.Load(ctx, "ses_v1_000_000_s1")
			if err != nil {
				t.Fatal(err)
			}
			events, err := eventlog.ReadAll(ctx, store.Log, "ses_v1_000_000_s1", 0)
			if err != nil {
				t.Fatal(err)
			}
			approvals := 0
			var res *v1.ToolResult
			for _, e := range events {
				switch {
				case e.GetApprovalRequested() != nil:
					approvals++
				case e.GetToolCallStarted() != nil:
					t.Fatal("memory call started before rejection or approval")
				case e.GetToolResult() != nil:
					res = e.GetToolResult()
				}
			}
			if n, err := mem.Store.Count(ctx, "bl", "u", clk.Now()); err != nil || n != 0 {
				t.Fatalf("memory count = %d, err = %v, want no writes", n, err)
			}
			r := final.Run("r1")
			if r == nil || len(r.Calls) != 1 {
				t.Fatalf("run = %v, want one memory call", r)
			}
			if tc.wantError == "" {
				if approvals != 1 || res != nil || r.Status != session.RunSuspended {
					t.Fatalf("approvals = %d, result = %v, run = %s, want waiting for approval", approvals, res, r.Status)
				}
				return
			}
			if approvals != 0 {
				t.Fatalf("requested %d approvals for rejected content", approvals)
			}
			if res == nil || !res.GetIsError() || model.Text(res.GetContent()) != checked.Error() {
				t.Fatalf("result = %v, want Check error %q", res, checked.Error())
			}
			if r.Status != session.RunCompleted {
				t.Fatalf("run = %s, want completed after the error was returned to the model", r.Status)
			}
		})
	}
}

// 空参数按 {} 处理，照常执行。
func TestEmptyArgumentsMeanEmptyObject(t *testing.T) {
	for _, args := range []string{"", "  \n"} {
		final, _, invoked := runCall(t, "note", args)
		if r := final.Run("r1"); r.Status != session.RunCompleted {
			t.Fatalf("run %s, want completed", r.Status)
		}
		if invoked != 1 {
			t.Fatalf("args %q: executed %d times, want 1", args, invoked)
		}
	}
}
