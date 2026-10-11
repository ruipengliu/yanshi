// Package decision 是判断层（docs/design/m6-decisions.md，ADR-0030）：对"给定状态、有限合法选项"的问题，由规则、
// 小模型或 LLM 作出选择。判断只缩小候选、追加约束或触发检查，从不放宽安全要求；影响后续执行的判断先记为
// DecisionMade 再据此行动，失败时走默认路径（不做任何优化，也不放行）。
package decision

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
)

// 判断点。
const (
	// ToolResultInjection 判断一条调用结果里是否含有针对 Agent 的提示注入（m6-decisions §3）。
	ToolResultInjection = "tool_result_injection"
	Clean               = "clean"
	Suspicious          = "suspicious"
)

// Question 是一次判断的输入。Text 是被判断的状态文本，含用户内容，只在内存中使用，不写日志（ADR-0015）。
type Question struct {
	Kind    string
	Subject string
	Text    string
	Options []string
}

// Answer 是一次判断的结果。
type Answer struct {
	Chosen     string
	Confidence float64
	// Model 是作出判断的规则集或模型引用。
	Model string
	// Usage 非空表示判断调用了模型，须计量（ADR-0019）；Billed 是这次调用的模型引用。二者与 Model 分开：
	// 小模型置信度不足、由规则作出判断时，用量仍属于小模型。
	Usage  *v1.Usage
	Billed string
}

// Decider 作出判断。返回错误表示判断不可用，调用方走默认路径；即使出错，返回的 Answer 也带着已产生的用量
// （Usage、Billed），调用方照常计量。

type Decider interface {
	Decide(ctx context.Context, q Question) (Answer, error)
}

// ErrUnsupported 表示判断器不支持这一判断点。
var ErrUnsupported = errors.New("decision: unsupported kind")

// InjectionRules 是注入检测的规则层：命中典型的劫持措辞即判为可疑。确定、零成本，是小模型不可用时的退路；
// 漏检多，只用于影子模式与收紧（不据此放行任何东西）。
type InjectionRules struct{}

// injectionPhrases 是典型的劫持措辞（小写比较）。
var injectionPhrases = []string{
	"ignore previous instructions", "ignore all previous", "ignore the above", "disregard previous", "disregard all prior",
	"you are now", "new instructions:", "system prompt", "do not tell the user", "developer mode",
	"忽略之前", "忽略以上", "忽略上面", "忽略前面", "无视之前", "无视以上", "你现在是", "新的指令", "系统提示词",
	"不要告诉用户", "不要让用户知道", "请立即执行以下", "作为ai助手你必须",
}

func (InjectionRules) Decide(_ context.Context, q Question) (Answer, error) {
	if q.Kind != ToolResultInjection {
		return Answer{}, fmt.Errorf("%w %q", ErrUnsupported, q.Kind)
	}
	text := strings.ToLower(q.Text)
	for _, p := range injectionPhrases {
		if strings.Contains(text, p) {
			return Answer{Chosen: Suspicious, Confidence: 0.9, Model: "rules/injection-v1"}, nil
		}
	}
	return Answer{Chosen: Clean, Confidence: 0.6, Model: "rules/injection-v1"}, nil
}

// Logits 是小模型层（System 1）：一次前向传播，读首个输出位置上选项标签（A、B、…）的对数概率，在合法选项上
// 归一化（m6-decisions §4）。置信度低于 MinConfidence 时交给 Fallback；模型不返回 logprobs 时报错。
type Logits struct {
	Provider model.Provider
	// Model 是 "provider/model" 引用，须是返回 logprobs 的自托管推理端点。
	Model string
	// Prompts 是各判断点的说明。
	Prompts       map[string]string
	MinConfidence float64
	Fallback      Decider
}

const labels = "ABCDEFGH"

func (l *Logits) Decide(ctx context.Context, q Question) (Answer, error) {
	prompt, ok := l.Prompts[q.Kind]
	if !ok || len(q.Options) > len(labels) {
		return Answer{}, fmt.Errorf("%w %q", ErrUnsupported, q.Kind)
	}
	var b strings.Builder
	b.WriteString(q.Text)
	b.WriteString("\n\n选项：\n")
	for i, o := range q.Options {
		fmt.Fprintf(&b, "%c. %s\n", labels[i], o)
	}
	b.WriteString("只回答一个选项字母。")
	resp, err := l.Provider.Generate(ctx, &model.Request{Model: l.Model, System: prompt, MaxTokens: 1, TopLogprobs: 20,
		Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks(b.String())}}}, nil)
	if err != nil {
		return Answer{}, err
	}
	billed := Answer{Usage: resp.Usage, Billed: l.Model}
	if len(resp.Logprobs) == 0 {
		return billed, fmt.Errorf("decision: model %s returned no logprobs", l.Model)
	}
	probs := make([]float64, len(q.Options))
	total := 0.0
	for tok, lp := range resp.Logprobs[0] {
		t := strings.TrimSpace(tok)
		if len(t) != 1 {
			continue
		}
		if i := strings.IndexByte(labels, t[0]); i >= 0 && i < len(probs) {
			probs[i] += math.Exp(lp)
			total += math.Exp(lp)
		}
	}
	if total == 0 {
		return billed, fmt.Errorf("decision: model %s put no probability on the option labels", l.Model)
	}
	best := 0
	for i := range probs {
		if probs[i] > probs[best] {
			best = i
		}
	}
	a := Answer{Chosen: q.Options[best], Confidence: probs[best] / total, Model: l.Model, Usage: resp.Usage, Billed: l.Model}
	if a.Confidence < l.MinConfidence && l.Fallback != nil {
		fb, err := l.Fallback.Decide(ctx, q)
		fb.Usage, fb.Billed = a.Usage, a.Billed
		return fb, err
	}
	return a, nil
}

// Guarded 给判断加上时限与指标。判断在 Worker 的一步内同步进行，时限保证它不拖住这一步。
type Guarded struct {
	Decider Decider
	// Timeout 默认 2 秒。
	Timeout time.Duration
}

func (g Guarded) Decide(ctx context.Context, q Question) (Answer, error) {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	a, err := g.Decider.Decide(ctx, q)
	metrics.DecisionDuration.WithLabelValues(q.Kind).Observe(metrics.Since(start))
	if err != nil {
		metrics.Decisions.WithLabelValues(q.Kind, "error").Inc()
		return Answer{Usage: a.Usage, Billed: a.Billed}, err
	}
	metrics.Decisions.WithLabelValues(q.Kind, a.Chosen).Inc()
	return a, nil
}

// ErrOpen 表示判断器已熔断：最近连续失败，冷却期内不再调用。
var ErrOpen = errors.New("decision: circuit open")

// Breaker 在判断连续失败 Failures 次后熔断 Cooldown：期间直接返回 ErrOpen，调用方走默认路径。端点宕机或迟缓时，
// 不让每一步都等到时限才退回（评审 §11 问题 5）。冷却期满后放行调用，再失败即重新熔断。
// 不支持的判断点与调用方取消不算失败。
type Breaker struct {
	Decider Decider
	// Failures 默认 3，Cooldown 默认 30 秒。
	Failures int
	Cooldown time.Duration
	Clock    clock.Clock

	mu        sync.Mutex
	fails     int
	openUntil time.Time
}

func (b *Breaker) Decide(ctx context.Context, q Question) (Answer, error) {
	now := b.Clock.Now()
	b.mu.Lock()
	open := now.Before(b.openUntil)
	b.mu.Unlock()
	if open {
		metrics.Decisions.WithLabelValues(q.Kind, "open").Inc()
		return Answer{}, ErrOpen
	}
	a, err := b.Decider.Decide(ctx, q)
	if errors.Is(err, ErrUnsupported) || ctx.Err() != nil {
		return a, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.fails = 0
		return a, nil
	}
	b.fails++
	failures, cooldown := b.Failures, b.Cooldown
	if failures <= 0 {
		failures = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if b.fails >= failures {
		b.fails, b.openUntil = 0, b.Clock.Now().Add(cooldown)
	}
	return a, err
}

// Options 返回判断点的合法选项（排序后的副本），未知的判断点返回 nil。
func Options(kind string) []string {
	switch kind {
	case ToolResultInjection:
		out := []string{Clean, Suspicious}
		sort.Strings(out)
		return out
	}
	return nil
}
