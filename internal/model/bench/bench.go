// Package bench 是压测用的模拟模型（docs/design/m2-scale-test.md §2）：不依赖外部服务，
// 以可配置的延迟模拟一次模型调用，并先发起若干轮工具调用再回复，使 Run 的形态接近真实 Agent。
//
// 模型 ID 形如 "latency=200ms,turns=2,tool=clock_now"：
//   - latency：每次调用的耗时，期间分段流式输出增量；
//   - ttft：首个正文增量的延迟，默认 latency/8，须在 0 与 latency 之间；
//   - turns：自最后一条用户消息起，先发起几轮工具调用；
//   - tool：调用的工具名，须在 AgentDef 的能力白名单中。
package bench

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

type Provider struct{}

type params struct {
	latency time.Duration
	ttft    time.Duration
	turns   int
	tool    string
}

func parse(id string) (params, error) {
	p := params{ttft: -1, latency: 200 * time.Millisecond, turns: 2, tool: "clock_now"}
	for _, kv := range strings.Split(id, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		var err error
		switch k {
		case "", "any":
		case "latency":
			p.latency, err = time.ParseDuration(v)
		case "ttft":
			p.ttft, err = time.ParseDuration(v)
			if err == nil && p.ttft < 0 {
				err = fmt.Errorf("ttft must be nonnegative")
			}
		case "turns":
			p.turns, err = strconv.Atoi(v)
		case "tool":
			p.tool = v
		default:
			err = fmt.Errorf("unknown parameter %q", k)
		}
		if err != nil {
			return p, fmt.Errorf("bench model %q: %w", id, err)
		}
	}
	if p.ttft < 0 {
		p.ttft = p.latency / chunks
	}
	if p.latency < 0 || p.ttft > p.latency {
		return p, fmt.Errorf("bench requires 0 <= ttft <= latency")
	}
	return p, nil
}

const chunks = 8

func (Provider) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	p, err := parse(req.Model)
	if err != nil {
		return nil, err
	}
	turns := 0
	for _, m := range req.Messages {
		switch m.Role {
		case model.RoleUser:
			turns = 0
		case model.RoleAssistant:
			turns++
		}
	}
	tool := turns < p.turns && hasTool(req, p.tool)
	start := time.Now()
	for i := range chunks {
		// 绝对时间避免分段取整和回调耗时逐次累积，最后一段仍落在 latency。
		due := p.ttft + time.Duration(i)*(p.latency-p.ttft)/(chunks-1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Until(start.Add(due))):
		}
		if onDelta != nil && !tool {
			onDelta(model.Delta{Text: fmt.Sprintf("片段%d ", i)})
		}
	}
	resp := &model.Response{Model: "bench/" + req.Model, Usage: &v1.Usage{
		InputTokens: uint64(model.EstimateRequest(req)), OutputTokens: chunks * 4,
	}}
	if tool {
		resp.ToolCalls = []*v1.ToolCall{{CallId: "c", Capability: p.tool, ArgumentsJson: "{}"}}
		return resp, nil
	}
	resp.Content = model.TextBlocks(strings.Repeat("片段 ", chunks))
	return resp, nil
}

func hasTool(req *model.Request, name string) bool {
	for _, t := range req.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
