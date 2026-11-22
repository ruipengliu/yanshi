package oneshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/usage"
)

const (
	DefaultMax      = 8
	DefaultWait     = 2 * time.Second
	DefaultDeadline = 60 * time.Second
	// SchemaInstruction 与修复说明是平台提示，不能拼入校验库返回的模型原文。
	SchemaInstruction = "\n只输出符合以下 JSON Schema 的 JSON 值，不要输出 Markdown 代码围栏或其他文字：\n"
	RepairInstruction = "\n上一次输出未通过校验，错误类别：%s。请根据原始输入重新生成符合上述 JSON Schema 的 JSON 值。"
)

var ErrInternal = errors.New("oneshot service unavailable")

// ValidateLimits 在启动时保证单次调用不能占满受限提供商；每个进程的容量须由部署静态分摊。
func ValidateLimits(max, providerMax int, wait, deadline time.Duration) error {
	if max <= 0 || (providerMax > 0 && max >= providerMax) {
		return errors.New("oneshot concurrency must be positive and below limited model concurrency")
	}
	if wait < 0 || deadline <= 0 {
		return errors.New("oneshot wait must be nonnegative and deadline must be positive")
	}
	return nil
}

type Request struct{ BusinessLine, EndUser, Name, Input string }

// Access 决定业务线可以使用哪些 Oneshot。
type Access interface {
	AllowsOneshot(businessLine, name string) bool
}

type Result struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Model   string `json:"model"`
	Output  any    `json:"output"`
	// Usage 仅供评测统计；失败时也保留已消耗的数量，不返回被拒绝的内容。
	Usage *v1.Usage `json:"-"`
}

// Service 由 app.NewCore 装配；Model、Clock、IDs 与正的 Max 必填，注册表在服务期间只热换发布选择。
type Service struct {
	Registry        *Registry
	Access          Access
	Model           model.Provider
	Moderator       moderation.Moderator
	Quotas          *usage.Quotas
	Meter           *usage.Meter
	Deletions       lifecycle.Deletions
	Clock           clock.Clock
	IDs             ids.Generator
	Max             int
	BusinessLineMax map[string]int
	Wait            time.Duration
	Deadline        time.Duration
	Slots           model.Slots
}

func (s *Service) Call(ctx context.Context, req Request) (result *Result, err error) {
	deadline := s.Deadline
	if deadline <= 0 {
		deadline = DefaultDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	// 所有退出路径只记声明与固定类别，不把任意请求名称放入指标标签。
	name, version := "", ""
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil && result != nil {
			result.Output = nil
		}
		metrics.OneshotCalls.WithLabelValues(name, version, outcome(err)).Inc()
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Access != nil && !s.Access.AllowsOneshot(req.BusinessLine, req.Name) {
		return nil, ErrNotFound
	}
	d, err := s.Registry.Resolve(req.Name, req.EndUser)
	if err != nil {
		return nil, ErrNotFound
	}
	name, version = d.Name, d.Version
	if req.BusinessLine == "" || req.EndUser == "" || req.EndUser == usage.Anonymous || strings.TrimSpace(req.Input) == "" || !utf8.ValidString(req.Input) || utf8.RuneCountInString(req.Input) > d.Input.MaxChars {
		return nil, ErrInvalid
	}
	if err := s.barrier(ctx, req); err != nil {
		return nil, err
	}
	if err := s.moderate(ctx, req.BusinessLine, moderation.Input, req.Input); err != nil {
		return nil, err
	}
	p, err := s.Quotas.Check(ctx, req.BusinessLine, req.EndUser, s.Clock.Now())
	if err != nil {
		return nil, ErrInternal
	}
	if p != nil {
		metrics.QuotaRejections.WithLabelValues(p.Scope, "oneshot").Inc()
		return nil, &usage.ExceededError{Period: p}
	}
	if s.Max <= 0 {
		return nil, ErrInternal
	}
	release, err := s.Slots.AcquireAll(ctx, []model.SlotReq{
		{Key: "total", Limit: s.Max, Wait: s.Wait},
		{Key: "business_line:" + req.BusinessLine, Limit: s.BusinessLineMax[req.BusinessLine], Wait: s.Wait},
		{Key: "name:" + req.BusinessLine + ":" + d.Name, Limit: d.Concurrency, Wait: s.Wait},
	})
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = model.WithOneshot(ctx)
	if d.WorkClass == "background" {
		ctx = model.WithBackground(ctx)
	}
	system := d.Instructions
	if d.HasSchema() {
		system += SchemaInstruction + d.schemaJSON
	}
	in := &model.Request{Model: d.Model, System: system, MaxTokens: d.MaxOutputTokens,
		Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks(req.Input)}}}
	result = &Result{Name: d.Name, Version: d.Version, Model: d.Model, Usage: &v1.Usage{}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		resp, callErr := s.Model.Generate(ctx, in, nil)
		if resp != nil {
			// token 在返回时已消耗；客户端取消不撤销计量，但不能延长本次调用的总时限。
			end, _ := ctx.Deadline()
			meterCtx, cancelMeter := context.WithDeadline(context.WithoutCancel(ctx), end)
			s.Meter.Oneshot(meterCtx, usage.Scope{BusinessLine: req.BusinessLine, EndUser: req.EndUser}, "use_"+s.IDs(), d.Label(), d.Model, resp.Usage, s.Clock.Now())
			cancelMeter()
			result.Usage.InputTokens += resp.Usage.GetInputTokens()
			result.Usage.OutputTokens += resp.Usage.GetOutputTokens()
			result.Usage.CachedInputTokens += resp.Usage.GetCachedInputTokens()
			metrics.ModelTokens.WithLabelValues("input").Add(float64(resp.Usage.GetInputTokens()))
			metrics.ModelTokens.WithLabelValues("output").Add(float64(resp.Usage.GetOutputTokens()))
			metrics.ModelTokens.WithLabelValues("cached_input").Add(float64(resp.Usage.GetCachedInputTokens()))
		}
		if callErr != nil {
			return result, modelError(ctx, callErr)
		}
		if resp == nil {
			return result, ErrProvider
		}
		text := model.Text(resp.Content)
		// 思考与意外的工具参数也不能绕过输出审核；它们不作为单次调用结果返回。
		review := text + resp.Reasoning
		if d.HasSchema() {
			var value any
			if json.Unmarshal([]byte(text), &value) == nil {
				// 展示 JSON 字段时会解码转义；送检同样覆盖解码后的文字。
				review += "\n" + jsonText(value)
			}
		}
		for _, call := range resp.ToolCalls {
			review += call.GetArgumentsJson()
		}
		if err := s.moderate(ctx, req.BusinessLine, moderation.Output, review); err != nil {
			return result, err
		}
		if len(resp.ToolCalls) != 0 {
			return result, ErrProvider
		}
		output, category := d.ValidateOutput(text)
		if category == "" {
			if err := s.barrier(ctx, req); err != nil {
				return result, err
			}
			result.Output = output
			return result, nil
		}
		if attempt == 1 {
			return result, ErrSchema
		}
		// 同一声明与原始输入再调用一次，不回送未通过校验的输出。
		in.System = system + fmt.Sprintf(RepairInstruction, category)
	}
	return result, ErrSchema
}

func (s *Service) barrier(ctx context.Context, req Request) error {
	deleted, err := lifecycle.EndUserDeleted(ctx, s.Deletions, req.BusinessLine, req.EndUser)
	if err != nil {
		return ErrInternal
	}
	if deleted {
		return ErrNotFound
	}
	return nil
}

func (s *Service) moderate(ctx context.Context, bl string, stage moderation.Stage, text string) error {
	v, err := moderation.Check(ctx, s.Moderator, moderation.Request{Stage: stage, BusinessLine: bl, Text: text})
	if err != nil {
		return moderation.ErrUnavailable
	}
	if v.Block {
		return moderation.ErrRejected
	}
	return nil
}

func modelError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, model.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, model.ErrBusy) {
		return model.ErrBusy
	}
	return ErrProvider
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, moderation.ErrRejected):
		return "rejected"
	case errors.Is(err, moderation.ErrUnavailable):
		return "moderation_unavailable"
	case errors.Is(err, usage.ErrExceeded):
		return "quota"
	case errors.Is(err, model.ErrBusy):
		return "busy"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrSchema):
		return "schema"
	default:
		return "error"
	}
}

// JSON 字段展示的是解码后的字符串；直接送检文字而不是再次序列化，以免 HTML 与控制字符重新转义。
func jsonText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var text []string
		for _, item := range v {
			text = append(text, jsonText(item))
		}
		return strings.Join(text, "\n")
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var text []string
		for _, key := range keys {
			text = append(text, key, jsonText(v[key]))
		}
		return strings.Join(text, "\n")
	default:
		return ""
	}
}
