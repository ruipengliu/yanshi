package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
)

func text(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

// categoryHelp 返回 JSON Schema 的枚举值与给模型看的类别说明。
func categoryHelp() (enum, help string) {
	var names, lines []string
	for _, c := range Categories {
		names = append(names, fmt.Sprintf("%q", c.Category))
		lines = append(lines, fmt.Sprintf("%s：%s", c.Category, c.Description))
	}
	return strings.Join(names, ","), strings.Join(lines, "；")
}

// Capabilities 返回 Agent 读写 Memory 的进程内能力（docs/design/m4-memory-grant.md §3）。
// 三者都是幂等的：memory_save 的 Memory ID 由调用 ID 派生，重复执行不会写入两条。
func Capabilities(s *Service) []capability.Capability {
	enum, help := categoryHelp()
	return []capability.Capability{
		capability.Func{
			S: capability.Spec{
				Name: "memory_save",
				Description: "记住关于用户的一条长期有用的信息，供以后的对话使用。一条只写一个事实，用简短的陈述句。" +
					"类别：" + help + "。过敏、饮食禁忌、慢性病与用药等健康信息关系到安全，用户提到时用类别 health 记住" +
					"（业务线未开启时会被拒绝，此时告诉用户无法记住）。不得保存金融账户、证件号码、精确位置等其他敏感个人信息，" +
					"也不要保存一次性的对话内容。" +
					"与已有信息冲突时，用 replaces 指定被替换的旧记忆 ID。",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"category":{"type":"string","enum":[` + enum + `]},` +
					`"content":{"type":"string"},"replaces":{"type":"string","description":"被替换的旧记忆 ID（可选）"}},"required":["category","content"]}`),
				Idempotent: true,
			},
			Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
				var in struct {
					Category Category `json:"category"`
					Content  string   `json:"content"`
					Replaces string   `json:"replaces"`
				}
				if err := json.Unmarshal([]byte(inv.Arguments), &in); err != nil {
					return nil, fmt.Errorf("invalid arguments: %w", err)
				}
				m, err := s.Save(ctx, SaveRequest{BusinessLine: inv.BusinessLine, EndUser: inv.EndUser, SessionID: inv.SessionID,
					CallID: inv.CallID, Category: in.Category, Content: in.Content, Replaces: in.Replaces})
				if err != nil {
					return nil, err
				}
				return text("已记住（" + m.ID + "）"), nil
			},
		},
		capability.Func{
			S: capability.Spec{
				Name:        "memory_forget",
				Description: "删除一条关于用户的记忆（只能删除本业务线写入的）。用户要求忘记某件事，或信息已经错误时使用。",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
				Idempotent:  true,
			},
			Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
				var in struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal([]byte(inv.Arguments), &in); err != nil {
					return nil, fmt.Errorf("invalid arguments: %w", err)
				}
				if err := s.Forget(ctx, inv.BusinessLine, inv.EndUser, in.ID); err != nil {
					return nil, err
				}
				return text("已删除 " + in.ID), nil
			},
		},
		capability.Func{
			S: capability.Spec{
				Name:        "memory_search",
				Description: "检索关于用户的记忆（含用户授权给本业务线读取的）。对话开始时已自动提供最相关的若干条，需要更多时再使用。",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
				Idempotent:  true,
			},
			Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
				var in struct {
					Query string `json:"query"`
				}
				if err := json.Unmarshal([]byte(inv.Arguments), &in); err != nil {
					return nil, fmt.Errorf("invalid arguments: %w", err)
				}
				hits, err := s.Search(ctx, inv.EndUser, inv.BusinessLine, in.Query, 10)
				if err != nil {
					return nil, err
				}
				if len(hits) == 0 {
					return text("没有相关记忆"), nil
				}
				var b strings.Builder
				for _, h := range hits {
					src := ""
					if h.BusinessLine != inv.BusinessLine {
						src = "，来自业务线 " + h.BusinessLine
					}
					fmt.Fprintf(&b, "- [%s%s] %s（%s）\n", h.Category, src, h.Content, h.ID)
				}
				return text(b.String()), nil
			},
		},
	}
}
