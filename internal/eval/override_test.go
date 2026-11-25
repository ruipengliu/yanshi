package eval

import (
	"slices"
	"testing"

	"yanshi/internal/agentdef"
)

// 评测换模型：内置循环换 model；声明了 loop.models 的外部循环以评测模型替换整个列表，且不改动注册表中的原定义；
// 没有声明的外部循环自带模型，没有可换的（ADR-0063 第 3 条）。
func TestOverrideModel(t *testing.T) {
	builtin := &agentdef.Def{Model: "p/a"}
	overrideModel(builtin, "p/x")
	if builtin.Model != "p/x" {
		t.Fatalf("builtin model %q", builtin.Model)
	}
	orig := &agentdef.LoopRef{Server: "plan", Models: []string{"p/a", "p/b"}}
	governed := &agentdef.Def{Loop: orig}
	overrideModel(governed, "p/x")
	if !slices.Equal(governed.Loop.Models, []string{"p/x"}) || !slices.Equal(orig.Models, []string{"p/a", "p/b"}) || governed.Model != "" {
		t.Fatalf("loop models %v, original %v", governed.Loop.Models, orig.Models)
	}
	own := &agentdef.Def{Loop: &agentdef.LoopRef{Server: "plan"}}
	overrideModel(own, "p/x")
	if len(own.Loop.Models) != 0 || own.Model != "" {
		t.Fatalf("self-hosted loop changed: %+v", own)
	}
	unchanged := &agentdef.Def{Model: "p/a"}
	overrideModel(unchanged, "")
	if unchanged.Model != "p/a" {
		t.Fatal("empty override changed the model")
	}
}
