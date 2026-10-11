package runtime

import (
	"context"
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

	call := &session.Call{Call: &v1.ToolCall{CallId: "c", Capability: "agent__parent", ArgumentsJson: `{"task":"x"}`}}
	if reason := w.delegationRefused(&session.Run{}, def, call); !strings.Contains(reason, "depth") {
		t.Fatalf("delegation beyond max depth: %q", reason)
	}
	bad := &session.Call{Call: &v1.ToolCall{CallId: "c", Capability: "agent__parent", ArgumentsJson: `{}`}}
	if reason := w.delegationRefused(&session.Run{}, def, bad); !strings.Contains(reason, "task is required") {
		t.Fatalf("delegation without a task: %q", reason)
	}
}
