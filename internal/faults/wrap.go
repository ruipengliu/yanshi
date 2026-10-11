package faults

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
)

// Model 在模型提供商处注入故障。serve 把它放在 model.Guard 之内：注入的限流与服务端错误同真实错误一样先经提供商层的
// 退避重试（计数一次的规则会被重试吸收，只在 yanshi_model_retries_total 上可见；计数更多时 Worker 才看到失败）。
type Model struct {
	Next     model.Provider
	Injector *Injector
}

func (m Model) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	if p, ok := m.Injector.Take(ctx, ModelDelay); ok {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(Param(p, "delay_ms", 3000)) * time.Millisecond):
		}
	}
	if _, ok := m.Injector.Take(ctx, ModelError); ok {
		return nil, &model.ProviderError{Model: req.Model, Status: 503, Code: "injected_fault"}
	}
	if p, ok := m.Injector.Take(ctx, ModelRateLimit); ok {
		return nil, &model.ProviderError{Model: req.Model, Status: 429, Code: "injected_fault",
			RetryAfter: time.Duration(Param(p, "retry_after", 2)) * time.Second}
	}
	p, cut := m.Injector.Take(ctx, ModelStreamCut)
	if !cut {
		return m.Next.Generate(ctx, req, onDelta)
	}
	// 转发增量直到 after_chars 个字符，然后像连接中断一样失败：订阅者已经看到了部分输出。
	limit, sent := Param(p, "after_chars", 20), 0
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := m.Next.Generate(ctx, req, func(d model.Delta) {
		if sent >= limit {
			cancel()
			return
		}
		if n := utf8.RuneCountInString(d.Text); sent+n > limit {
			d.Text = string([]rune(d.Text)[:limit-sent])
		}
		sent += utf8.RuneCountInString(d.Text)
		if onDelta != nil && d.Text != "" {
			onDelta(d)
		}
	})
	_ = err // 无论模型是否已经输出完，结果都作废
	return nil, fmt.Errorf("model %s: %w (injected fault after %d characters)", req.Model, model.ErrStalled, sent)
}

// Embed 原样转发嵌入请求（Memory 召回）：包装不能挡住被包装提供商的可选能力，否则开启 -faults 后召回一律失败。
func (m Model) Embed(ctx context.Context, modelID string, texts []string) ([][]float32, error) {
	e, ok := m.Next.(model.Embedder)
	if !ok {
		return nil, fmt.Errorf("model %s: provider does not support embeddings", modelID)
	}
	return e.Embed(ctx, modelID, texts)
}

var _ model.Embedder = Model{}

// Moderator 在内容安全提供商处注入"服务不可用"。
type Moderator struct {
	Next     moderation.Moderator
	Injector *Injector
}

func (m Moderator) Check(ctx context.Context, req moderation.Request) (moderation.Verdict, error) {
	if _, ok := m.Injector.Take(ctx, ModerationDown); ok {
		return moderation.Verdict{}, fmt.Errorf("moderation: injected fault (unavailable)")
	}
	if m.Next == nil {
		return moderation.Verdict{}, nil
	}
	return m.Next.Check(ctx, req)
}

// Directory 在设备目录的查询（解析工具）处注入暂时性故障；登记与上下线不受影响。
type Directory struct {
	node.Directory
	Injector *Injector
}

func (d Directory) List(ctx context.Context, scope node.Scope) ([]*node.Info, error) {
	if _, ok := d.Injector.Take(ctx, CatalogError); ok {
		return nil, fmt.Errorf("node directory: injected fault (unavailable)")
	}
	return d.Directory.List(ctx, scope)
}
