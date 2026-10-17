// Package script 是脚本化模型：不依赖外部服务，按用户输入中的命令发起工具调用或流式输出，结果确定。
// 用于端到端测试与调试控制台（/console）：在没有模型密钥时演示与验证能力路由、审批、挂起、提问、插话与中断。
//
// 命令写在用户输入中，每行一条（其余各行忽略）：
//
//	call <工具名后缀> [JSON 参数]   调用名称等于它的工具，没有时调用名称以它结尾的第一个工具，如
//	                                "call read_file {"path":"a.txt"}" 匹配 "macbook__read_file"；多行 call 依次执行
//	ask <问题> [| 选项 | 选项…]      经 ask_user 向用户提问（ADR-0025）
//	stream <n>                      全部调用完成后，在约 n×20ms 内逐段流式输出 n 段
//	think <n>                       每次模型调用先流式输出 n 段思考过程（ADR-0034），约 n×20ms
//	together                        全部 call 与 ask 在同一次模型调用中一起返回（一轮多个调用），而不是依次各占一轮
//
// 有调用时，全部完成后回复 "done: <最后一个结果>"（有 stream 时改为流式输出）；没有命令时复述输入（"echo: …"）。
// 插话开启新的命令序列：最后一条用户输入才是当前的脚本。后台调用的了结与取消（"[后台调用…]"）不是命令，复述它。
package script

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

// ChunkInterval 是 stream 命令每段之间的间隔。
const ChunkInterval = 20 * time.Millisecond

// MaxStream 是 stream 命令的段数上限（约 2 分钟）。
const MaxStream = 6000

type Provider struct{}

type command struct {
	tool string // 工具名后缀
	args string
}

type plan struct {
	calls  []command
	stream int
	think  int
	text   string
	// together：剩下的调用在同一次模型调用中一起返回。用于检查 Worker 按顺序推进同一轮的多个调用时客户端的表现
	// （例如委托与 ask_user 同一轮：提问要等委托结束才派发）。
	together bool
}

func (Provider) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	input, results := current(req.Messages)
	p := parse(input)
	if strings.HasPrefix(input, "[后台调用") {
		// 后台调用的了结与取消由平台送达（ADR-0035）：其中是设备返回的数据，不是命令，复述它。
		p = plan{text: strings.TrimSpace(input)}
	}
	resp := &model.Response{Model: "script/" + req.Model, Usage: &v1.Usage{InputTokens: uint64(model.EstimateRequest(req))}}
	if err := think(ctx, resp, p.think, onDelta); err != nil {
		return nil, err
	}
	if len(results) < len(p.calls) {
		next := p.calls[len(results) : len(results)+1]
		if p.together {
			next = p.calls[len(results):]
		}
		for _, c := range next {
			name := match(req.Tools, c.tool)
			if name == "" {
				return reply(ctx, resp, "no such tool: "+c.tool, 0, onDelta)
			}
			// 调用 ID 由 Worker 改写为全局唯一的 ID。
			resp.ToolCalls = append(resp.ToolCalls, &v1.ToolCall{CallId: "x", Capability: name, ArgumentsJson: c.args})
		}
		resp.FinishReason = "tool_calls"
		return resp, nil
	}
	switch {
	case p.stream > 0:
		return reply(ctx, resp, "", p.stream, onDelta)
	case len(p.calls) > 0:
		return reply(ctx, resp, "done: "+results[len(results)-1], 0, onDelta)
	default:
		return reply(ctx, resp, "echo: "+p.text, 0, onDelta)
	}
}

// match 返回名称等于 suffix 的工具，没有时返回名称以它结尾的第一个工具。
func match(tools []model.ToolSpec, suffix string) string {
	name := ""
	for _, t := range tools {
		if t.Name == suffix {
			return t.Name
		}
		if name == "" && strings.HasSuffix(t.Name, suffix) {
			name = t.Name
		}
	}
	return name
}

// current 返回当前脚本所在的用户输入，以及其后已收到的调用结果（按顺序）。Call 的转写（"[语音通话] …"）
// 不是给脚本的命令，跳过。
func current(msgs []model.Message) (string, []string) {
	var results []string
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		switch m.Role {
		case model.RoleTool:
			results = append([]string{model.Text(m.Content)}, results...)
		case model.RoleUser:
			if t := userText(m.Content); !strings.HasPrefix(t, "[语音通话]") {
				return t, results
			}
		}
	}
	return "", results
}

// userText 是用户输入中的文字；卡片动作与真实提供商一样呈现为一行文字（model.CardActionText），脚本复述它。
func userText(blocks []*v1.ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		if a := c.GetCardAction(); a != nil {
			b.WriteString(model.CardActionText(a))
		}
		b.WriteString(c.GetText().GetText())
	}
	return b.String()
}

func parse(input string) plan {
	p := plan{text: strings.TrimSpace(input)}
	for line := range strings.SplitSeq(input, "\n") {
		line = strings.TrimSpace(line)
		verb, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch verb {
		case "call":
			tool, args, _ := strings.Cut(rest, " ")
			if args = strings.TrimSpace(args); args == "" {
				args = "{}"
			}
			if tool != "" {
				p.calls = append(p.calls, command{tool: tool, args: args})
			}
		case "ask":
			p.calls = append(p.calls, command{tool: "ask_user", args: askArgs(rest)})
		case "stream":
			if n, err := strconv.Atoi(rest); err == nil && n > 0 {
				p.stream = min(n, MaxStream)
			}
		case "think":
			if n, err := strconv.Atoi(rest); err == nil && n > 0 {
				p.think = min(n, MaxStream)
			}
		case "together":
			p.together = true
		}
	}
	return p
}

// askArgs 把 "问题 | 选项 | 选项" 转为 ask_user 的参数；选项 ID 依次为 "1"、"2"…
func askArgs(s string) string {
	parts := strings.Split(s, "|")
	a := struct {
		Question string `json:"question"`
		Options  []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options,omitempty"`
	}{Question: strings.TrimSpace(parts[0])}
	for i, o := range parts[1:] {
		if o = strings.TrimSpace(o); o != "" {
			a.Options = append(a.Options, struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			}{strconv.Itoa(i + 1), o})
		}
	}
	b, _ := json.Marshal(a)
	return string(b)
}

// reply 回复文本：stream > 0 时按 ChunkInterval 逐段输出 "chunk<i> "，最终内容为 "streamed"；否则逐字输出 text。
func reply(ctx context.Context, resp *model.Response, text string, stream int, onDelta func(model.Delta)) (*model.Response, error) {
	if stream > 0 {
		for i := range stream {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(ChunkInterval):
			}
			if onDelta != nil {
				onDelta(model.Delta{Text: fmt.Sprintf("chunk%d ", i)})
			}
		}
		text = "streamed"
	} else if onDelta != nil {
		for s := text; s != ""; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			_, n := utf8.DecodeRuneInString(s)
			onDelta(model.Delta{Text: s[:n]})
			s = s[n:]
		}
	}
	resp.Content = model.TextBlocks(text)
	resp.Usage.OutputTokens = uint64(model.EstimateTokens(resp.Content)) + resp.Usage.GetReasoningTokens()
	resp.FinishReason = "stop"
	return resp, nil
}

// think 按 ChunkInterval 逐段输出思考过程 "思考第 i 步。"，汇总到 resp.Reasoning。
func think(ctx context.Context, resp *model.Response, n int, onDelta func(model.Delta)) error {
	var b strings.Builder
	for i := range n {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ChunkInterval):
		}
		s := fmt.Sprintf("思考第 %d 步。", i+1)
		b.WriteString(s)
		if onDelta != nil {
			onDelta(model.Delta{Reasoning: s})
		}
	}
	resp.Reasoning = b.String()
	resp.Usage.ReasoningTokens = uint64(model.EstimateText(resp.Reasoning))
	return nil
}
