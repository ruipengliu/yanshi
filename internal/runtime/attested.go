package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/moderation"
)

// attestedPage 与 SubmitCaptures 使用相同的整批、逐段、组合违规兜底规则；不把提供商错误带进日志。
func (w *Worker) attestedPage(ctx context.Context, inv capability.Invocation, raw json.RawMessage) ([]*v1.ContentBlock, error) {
	opts := capture.OptionsFor(inv)
	page, err := capture.ParsePage(raw, opts)
	if err != nil {
		return nil, err
	}
	if _, _, err := capture.Render(page, opts.MaxToolResult); err != nil {
		return nil, err
	}
	if w.cfg.Moderator != nil && len(page.Segments) > 0 {
		check := func(text string) (bool, error) {
			v, err := moderation.Check(ctx, w.cfg.Moderator, moderation.Request{Stage: moderation.Input, BusinessLine: inv.BusinessLine, SessionID: inv.SessionID, Text: text})
			return v.Block, err
		}
		texts := make([]string, len(page.Segments))
		for i, g := range page.Segments {
			texts[i] = g.GetText()
		}
		block, err := check(strings.Join(texts, "\n"))
		if err != nil {
			return nil, fmt.Errorf("旁听页的内容安全检查暂不可用，稍后再读")
		}
		if block {
			blocked, n := make([]bool, len(texts)), 0
			for i, text := range texts {
				blocked[i], err = check(text)
				if err != nil {
					return nil, fmt.Errorf("旁听页的内容安全检查暂不可用，稍后再读")
				}
				if blocked[i] {
					n++
				}
			}
			for i, g := range page.Segments {
				if blocked[i] || n == 0 {
					g.Text, g.Redacted = "", true
				}
			}
		}
	}
	blocks, _, err := capture.Render(page, opts.MaxToolResult)
	return blocks, err
}
