package runtime

import (
	"context"

	"yanshi/internal/model"
)

// Loop 决定 Run 的下一步：给出回复与调用请求（CONTEXT.md 的 Loop，ADR-0041）。它只做决定：上下文组装、工具解析与
// 祖先交集、配额、压缩、调用 ID、内容安全与写日志都留在 Worker 中，不区分循环是内置的还是外部的（ADR-0062 第 9 条）。
// 外部循环尚未实现；内置实现 ModelLoop 把平台组装的请求原样交给模型，请求逐字节不变，前缀缓存照常命中。
type Loop interface {
	// Decide 给出这一步的回复。onDelta 接收流式增量，只用于实时推送、不写日志。返回的回复由 Worker 改写调用 ID、
	// 送检后写入日志；返回错误按模型错误处理（MaxModelErrors，上下文超长时下一步先压缩）。
	Decide(ctx context.Context, step LoopStep, onDelta func(model.Delta)) (*model.Response, error)
}

// LoopStep 是一步的输入。外部循环需要的 Run、Attempt、不透明状态与上一步的调用结果（ADR-0062）在实现时加入。
type LoopStep struct {
	// Request 是平台组装的模型请求：系统指令只含 AgentDef 的指令，召回在 Run 的位置上，Run 内的上下文只在尾部追加。
	Request *model.Request
}

// ModelLoop 是内置循环：调用模型。
type ModelLoop struct{ Model model.Provider }

func (l ModelLoop) Decide(ctx context.Context, step LoopStep, onDelta func(model.Delta)) (*model.Response, error) {
	return l.Model.Generate(ctx, step.Request, onDelta)
}
