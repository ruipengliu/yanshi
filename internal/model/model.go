// Package model 是模型网关：业务与运行时只依赖这里的抽象，
// 具体供应商（火山方舟、私有化推理服务等）以 Provider 接入。
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role    Role
	Content []*v1.ContentBlock
	// ToolCalls 仅用于 RoleAssistant。
	ToolCalls []*v1.ToolCall
	// ToolCallID 仅用于 RoleTool。
	ToolCallID string
}

type ToolSpec struct {
	Name        string
	Description string
	// InputSchema 是 JSON Schema 对象。
	InputSchema json.RawMessage
}

type Request struct {
	// Model 是供应商内的模型 ID（不含供应商前缀）。
	Model    string
	System   string
	Messages []Message
	Tools    []ToolSpec
}

type Response struct {
	Content   []*v1.ContentBlock
	ToolCalls []*v1.ToolCall
	Usage     *v1.Usage
	Model     string
}

// Delta 是流式输出的一段增量文本。
type Delta struct {
	Text string
}

type Provider interface {
	// Generate 调用模型；onDelta 可为 nil，否则在流式增量到达时同步调用。
	Generate(ctx context.Context, req *Request, onDelta func(Delta)) (*Response, error)
}

// Embedder 是可选的 Provider 能力：计算文本的嵌入向量（Memory 检索，docs/design/m4-memory-grant.md §5）。
type Embedder interface {
	Embed(ctx context.Context, model string, texts []string) ([][]float32, error)
}

// Gateway 按 "provider/model" 形式的模型引用把请求路由到对应 Provider。
type Gateway struct {
	providers map[string]Provider
}

func NewGateway() *Gateway { return &Gateway{providers: map[string]Provider{}} }

func (g *Gateway) Register(name string, p Provider) { g.providers[name] = p }

// SplitRef 把 "provider/model" 拆开；model 部分可以包含 "/"。
func SplitRef(ref string) (provider, model string, err error) {
	provider, model, ok := strings.Cut(ref, "/")
	if !ok || provider == "" || model == "" {
		return "", "", fmt.Errorf("model ref %q: want provider/model", ref)
	}
	return provider, model, nil
}

// Generate 中 req.Model 是完整的 "provider/model" 引用。
func (g *Gateway) Generate(ctx context.Context, req *Request, onDelta func(Delta)) (*Response, error) {
	provider, m, err := SplitRef(req.Model)
	if err != nil {
		return nil, err
	}
	p, ok := g.providers[provider]
	if !ok {
		return nil, fmt.Errorf("model provider %q not configured", provider)
	}
	r := *req
	r.Model = m
	return p.Generate(ctx, &r, onDelta)
}

// Text 返回内容块中所有文本的拼接。
func Text(blocks []*v1.ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		b.WriteString(c.GetText().GetText())
	}
	return b.String()
}

// DescribeMedia 把非文本内容呈现为给模型看的一行文本，如 "[artifact art_x: chart.png, image/png, 7.6 KB]"。
func DescribeMedia(m *v1.Media) string {
	id, ok := strings.CutPrefix(m.GetUri(), "artifact://")
	if !ok || id == "" {
		id = "inline"
	}
	n := m.GetSize()
	size := fmt.Sprintf("%d B", n)
	switch {
	case n >= 1<<20:
		size = fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		size = fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("[artifact %s: %s, %s, %s]", id, m.GetName(), m.GetMimeType(), size)
}

// TextBlocks 把一段文本包装为内容块。
func TextBlocks(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

// Embed 中 model 是完整的 "provider/model" 引用；供应商须实现 Embedder。
func (g *Gateway) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	provider, m, err := SplitRef(model)
	if err != nil {
		return nil, err
	}
	p, ok := g.providers[provider]
	if !ok {
		return nil, fmt.Errorf("model provider %q not configured", provider)
	}
	e, ok := p.(Embedder)
	if !ok {
		return nil, fmt.Errorf("model provider %q does not support embeddings", provider)
	}
	return e.Embed(ctx, m, texts)
}
