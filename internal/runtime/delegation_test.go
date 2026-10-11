package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/session"
)

type namedCap string

func (c namedCap) Spec() capability.Spec { return capability.Spec{Name: string(c)} }
func (namedCap) Invoke(context.Context, capability.Invocation) ([]*v1.ContentBlock, error) {
	return nil, nil
}

type agentList []capability.AgentInfo

func (l agentList) Agents(string) []capability.AgentInfo { return l }

// 委托派生的子 Run 只能用自身白名单与每个祖先白名单都允许的工具；委托链深度与未完成的委托数受委托方的上限约束
// （docs/design/m5-multi-agent.md §2、§4）。
func TestDelegationLimits(t *testing.T) {
	ctx := context.Background()
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "parent", Version: "1", Model: "m/x", Capabilities: []string{"read", "agent:*"}},
		&agentdef.Def{Name: "child", Version: "1", Model: "m/x", Capabilities: []string{"read", "write", "agent:*"},
			Delegation: agentdef.DelegationConfig{MaxDepth: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &capability.Catalog{Local: capability.NewRegistry(namedCap("read"), namedCap("write")),
		Agents: agentList{{Name: "child", Timeout: time.Hour}, {Name: "parent", Timeout: time.Hour}}}
	w := New(Config{ID: "w", Agents: agents, Catalog: catalog})
	w.st = &session.State{SessionID: "s", Created: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u",
		Parent: &v1.ParentRef{SessionId: "p", Ancestors: []*v1.AgentRef{{Name: "parent", Version: "1"}}}}}
	def, _ := agents.Get(&v1.AgentRef{Name: "child", Version: "1"})
	tools, err := w.tools(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Spec.Name)
	}
	if got := strings.Join(names, ","); got != "read,agent__child,agent__parent" {
		t.Fatalf("child tools %s; write is not allowed by the parent", got)
	}

	// 祖先的版本不在注册表中：配置错误，而不是悄悄失去全部工具。
	orphan := New(Config{ID: "w", Agents: agents, Catalog: catalog})
	orphan.st = &session.State{SessionID: "o", Created: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u",
		Parent: &v1.ParentRef{SessionId: "p", Ancestors: []*v1.AgentRef{{Name: "parent", Version: "9"}}}}}
	if _, err := orphan.tools(ctx, def); !errors.Is(err, capability.ErrConfig) {
		t.Fatalf("missing ancestor version: %v", err)
	}

	call := &session.Call{Call: &v1.ToolCall{CallId: "c", Capability: "agent__parent", ArgumentsJson: `{"task":"x"}`}}
	if reason := w.delegationRefused(&session.Run{}, def, call); !strings.Contains(reason, "depth") {
		t.Fatalf("delegation beyond max depth: %q", reason)
	}
	// 上限含本层：max_depth 为 2 时顶层可以委托，第 2 层的子 Session 不能再委托（评审 §11 问题 9）。
	two := *def
	two.Delegation.MaxDepth = 2
	if reason := w.delegationRefused(&session.Run{}, &two, call); !strings.Contains(reason, "depth 3") {
		t.Fatalf("a depth-2 child delegated under max_depth 2: %q", reason)
	}
	top := New(Config{ID: "w", Agents: agents, Catalog: catalog})
	top.st = &session.State{SessionID: "t", Created: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}
	if reason := top.delegationRefused(&session.Run{}, &two, call); reason != "" {
		t.Fatalf("a top-level session could not delegate under max_depth 2: %q", reason)
	}
	bad := &session.Call{Call: &v1.ToolCall{CallId: "c", Capability: "agent__parent", ArgumentsJson: `{}`}}
	if reason := w.delegationRefused(&session.Run{}, def, bad); !strings.Contains(reason, "task is required") {
		t.Fatalf("delegation without a task: %q", reason)
	}
}
