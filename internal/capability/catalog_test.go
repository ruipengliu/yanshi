package capability_test

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/node"
)

// countingDir 统计 List 的次数。
type countingDir struct {
	node.Directory
	lists int
}

func (d *countingDir) List(ctx context.Context, scope node.Scope) ([]*node.Info, error) {
	d.lists++
	return d.Directory.List(ctx, scope)
}

// TestNodeListCached：同一 EndUser 的 Node 列表在 NodeCacheTTL 内只查一次，过期后重新查询并看到新登记的设备。
func TestNodeListCached(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	dir := &countingDir{Directory: node.NewMemDirectory(clk)}
	scope := node.Scope{BusinessLine: "bl", EndUser: "u"}
	register := func(id, label string) {
		t.Helper()
		if _, _, err := dir.Register(ctx, node.Info{NodeID: id, Scope: scope, Label: label, Kind: "phone",
			Capabilities: []*v1.CapabilitySpec{{Name: "read_file", InputSchemaJson: `{"type":"object"}`}}}); err != nil {
			t.Fatal(err)
		}
	}
	register("n1", "phone")
	c := &capability.Catalog{Local: capability.NewRegistry(), Nodes: dir, NodeCacheTTL: 10 * time.Second, Clock: clk}
	target := capability.Target{Scope: scope, SessionID: "s"}
	tools := func() int {
		t.Helper()
		ts, err := c.Tools(ctx, target, []string{"device:*"})
		if err != nil {
			t.Fatal(err)
		}
		return len(ts)
	}
	if tools() != 1 || tools() != 1 {
		t.Fatal("want one device tool")
	}
	if tool, err := c.Resolve(ctx, target, []string{"device:*"}, "phone__read_file"); err != nil || tool == nil {
		t.Fatalf("resolve = %v, %v", tool, err)
	}
	if dir.lists != 1 {
		t.Fatalf("listed nodes %d times within the TTL", dir.lists)
	}
	register("n2", "laptop")
	if tools() != 1 {
		t.Fatal("new device visible before the cache expired")
	}
	clk.Advance(10 * time.Second)
	if tools() != 2 || dir.lists != 2 {
		t.Fatalf("after TTL: lists %d", dir.lists)
	}
}

// 设备与沙箱即使低风险、幂等，也不能把声明转换为只读证据。
func TestRoutedToolsHaveNoReadOnlyEvidence(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	dir := node.NewMemDirectory(clk)
	scope := node.Scope{BusinessLine: "bl", EndUser: "u"}
	spec := &v1.CapabilitySpec{Name: "read_file", Idempotent: true, Risk: v1.Risk_RISK_LOW, InputSchemaJson: `{"type":"object"}`}
	if _, _, err := dir.Register(ctx, node.Info{NodeID: "n1", Scope: scope, Label: "phone", Kind: "phone", Capabilities: []*v1.CapabilitySpec{spec}}); err != nil {
		t.Fatal(err)
	}
	cat := &capability.Catalog{
		Local: capability.NewRegistry(capability.ClockNow(clk)), Nodes: dir,
		Sandbox: &capability.SandboxTools{Specs: []*v1.CapabilitySpec{spec}, NodeID: func(string) string { return "sbx_s" }},
	}
	tools, err := cat.Tools(ctx, capability.Target{Scope: scope, SessionID: "s"}, []string{"clock_now", "ask_user", "device:*", "sandbox:*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("tools = %v", tools)
	}
	for _, tool := range tools {
		if want := tool.Spec.Name == "clock_now"; tool.Spec.ReadOnly != want {
			t.Errorf("%s ReadOnly = %v, want %v", tool.Spec.Name, tool.Spec.ReadOnly, want)
		}
	}
}
