package model

import (
	"errors"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
)

// ErrContextOverflow 表示请求超出了模型的上下文窗口。Provider 应把供应商的对应错误包装为它，
// 运行时据此强制压缩上下文（docs/design/m2-long-runs.md §3）。
var ErrContextOverflow = errors.New("context window exceeded")

// 估算参数。估算必须是确定性的纯函数（模拟测试与重放依赖它），并且宁可偏高：
// 中文 UTF-8 每字 3 字节、通常不超过 1 个 token；英文每 token 约 4 字节。
const (
	bytesPerToken      = 3
	messageOverhead    = 4
	inlineImageTokens  = 1500
	mediaRefTokens     = 24
	toolOverheadTokens = 8
)

// EstimateText 估算一段文本的 token 数。
func EstimateText(s string) int { return (len(s) + bytesPerToken - 1) / bytesPerToken }

// EstimateTokens 估算内容块的 token 数。
func EstimateTokens(blocks []*v1.ContentBlock) int {
	n := 0
	for _, b := range blocks {
		switch k := b.GetKind().(type) {
		case *v1.ContentBlock_Text:
			n += EstimateText(k.Text.GetText())
		case *v1.ContentBlock_UiContext:
			n += EstimateText(UIContextText(k.UiContext))
		case *v1.ContentBlock_Media:
			if len(k.Media.GetData()) > 0 {
				n += inlineImageTokens
			} else {
				n += mediaRefTokens
			}
		}
	}
	return n
}

// EstimateMessage 估算一条消息的 token 数。
func EstimateMessage(m Message) int {
	n := messageOverhead + EstimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += toolOverheadTokens + EstimateText(tc.GetCapability()) + EstimateText(tc.GetArgumentsJson())
	}
	return n
}

// EstimateRequest 估算整个请求（系统指令、工具声明与消息）的 token 数。
func EstimateRequest(req *Request) int {
	n := EstimateText(req.System)
	for _, t := range req.Tools {
		n += toolOverheadTokens + EstimateText(t.Name) + EstimateText(t.Description) + EstimateText(string(t.InputSchema))
	}
	for _, m := range req.Messages {
		n += EstimateMessage(m)
	}
	return n
}

// TruncateText 把 s 截断到约 maxTokens 个 token（按估算），截断处落在字符边界上。
func TruncateText(s string, maxTokens int) (string, bool) {
	limit := maxTokens * bytesPerToken
	if len(s) <= limit {
		return s, false
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit], true
}
