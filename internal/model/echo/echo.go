// Package echo 是不依赖外部服务的 Provider：复述最后一条用户输入。
// 用于离线开发与演示单二进制模式。
package echo

import (
	"context"
	"strings"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

type Provider struct{}

func (Provider) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	var last string
	for _, m := range req.Messages {
		if m.Role == model.RoleUser {
			last = model.Text(m.Content)
		}
	}
	out := "echo: " + last
	if onDelta != nil {
		for s := out; s != ""; {
			_, n := utf8.DecodeRuneInString(s)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			onDelta(model.Delta{Text: s[:n]})
			s = s[n:]
		}
	}
	return &model.Response{
		Content: model.TextBlocks(out),
		Model:   "echo/" + req.Model,
		Usage:   &v1.Usage{InputTokens: uint64(len(strings.Fields(last))), OutputTokens: uint64(len(strings.Fields(out)))},
	}, nil
}
