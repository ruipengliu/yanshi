// Package capability 定义 Agent 可调用的 Capability。
//
// M0 只有进程内 Capability；M1 起由能力路由把调用分发到 Device、Sandbox 等 Node，
// 但对运行时暴露的仍是这里的接口。
package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	v1 "yanshi/gen/yanshi/v1"
)

type Spec struct {
	// Name 须匹配 ^[a-zA-Z0-9_-]+$（模型工具名约束）。
	Name        string
	Description string
	InputSchema json.RawMessage
	// Idempotent 为 true 时，执行结果未知的调用可在故障恢复后重新执行。
	Idempotent bool
	// Risk 为 RISK_HIGH 时，执行前需 EndUser 审批。
	Risk v1.Risk
}

// Invocation 是一次调用的输入。CallID 在 Session 内唯一，跨重试不变，
// 可作为下游的幂等键。
type Invocation struct {
	SessionID string
	RunID     string
	CallID    string
	Arguments string
	// BusinessLine 与 EndUser 是 Session 的所属方，供按用户隔离数据的能力使用（如 Memory）。
	BusinessLine string
	EndUser      string
}

type Capability interface {
	Spec() Spec
	// Invoke 执行调用；返回的 error 会作为错误结果交给模型。
	Invoke(ctx context.Context, inv Invocation) ([]*v1.ContentBlock, error)
}

type Registry struct {
	caps map[string]Capability
}

func NewRegistry(caps ...Capability) *Registry {
	r := &Registry{caps: map[string]Capability{}}
	for _, c := range caps {
		r.caps[c.Spec().Name] = c
	}
	return r
}

func (r *Registry) Get(name string) (Capability, bool) {
	c, ok := r.caps[name]
	return c, ok
}

// Specs 返回 names 对应的 Spec，按名称排序；未知名称返回错误。
func (r *Registry) Specs(names []string) ([]Spec, error) {
	out := make([]Spec, 0, len(names))
	for _, n := range names {
		c, ok := r.caps[n]
		if !ok {
			return nil, fmt.Errorf("unknown capability %q", n)
		}
		out = append(out, c.Spec())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Func 用函数实现 Capability。
type Func struct {
	S  Spec
	Fn func(ctx context.Context, inv Invocation) ([]*v1.ContentBlock, error)
}

func (f Func) Spec() Spec { return f.S }
func (f Func) Invoke(ctx context.Context, inv Invocation) ([]*v1.ContentBlock, error) {
	return f.Fn(ctx, inv)
}
