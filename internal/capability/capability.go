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
	"yanshi/internal/agentdef"
	"yanshi/internal/session"
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
	// State 与 Agent 是调用时的只读投影与 AgentDef，供可信页预算和引用上界使用；测试可为空。
	State *session.State
	Agent *agentdef.Def
}

// Result 是一次进程内调用的完整结果，可信页由平台审核和渲染。
type Result struct {
	Content []*v1.ContentBlock
	// Page 是经同业务线登记筛选的结构化页，Worker 校验、审核并渲染。
	Page json.RawMessage
}

// ResultInvoker 是可选的 Capability 能力：除内容外还给出只由平台写入 ToolResult 的字段。Worker 优先调用它。
type ResultInvoker interface {
	InvokeResult(ctx context.Context, inv Invocation) (Result, error)
}

type Capability interface {
	Spec() Spec
	// Invoke 执行调用；返回的 error 会作为错误结果交给模型。
	Invoke(ctx context.Context, inv Invocation) ([]*v1.ContentBlock, error)
}

// Checker 是可选的 Capability 能力：执行之前、不依赖外部状态也没有副作用的检查。Worker 在审批与 Memory 写入闸门之前
// 调用它：注定被拒绝的调用不去打扰用户（与参数 JSON 的检查同理），返回的 error 作为错误结果交还模型。
type Checker interface {
	Check(inv Invocation) error
}

// ApprovalSummarizer 为受写入闸门约束的本地能力提供用户可读的动作摘要。
// 空摘要表示只读动作；是否需要审批仍由 Worker 判断。
type ApprovalSummarizer interface {
	ApprovalSummary(inv Invocation) string
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

// Func 用函数实现 Capability。Pre 非空时是执行之前的检查（Checker）。
type Func struct {
	S   Spec
	Fn  func(ctx context.Context, inv Invocation) ([]*v1.ContentBlock, error)
	Pre func(inv Invocation) error
}

func (f Func) Spec() Spec { return f.S }

func (f Func) Check(inv Invocation) error {
	if f.Pre == nil {
		return nil
	}
	return f.Pre(inv)
}
func (f Func) Invoke(ctx context.Context, inv Invocation) ([]*v1.ContentBlock, error) {
	return f.Fn(ctx, inv)
}
