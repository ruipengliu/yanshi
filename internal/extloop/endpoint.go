package extloop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/tracing"
	"yanshi/internal/usage"
)

// EndpointPath 是模型端点的路径。外部服务把 base URL 配成 <yanshi>/v1/loop，OpenAI 兼容的客户端在其后接
// /chat/completions。它不经 yanshi 的令牌中间件：只接受步令牌（ADR-0063 第 2 条）。
const EndpointPath = "/v1/loop/chat/completions"

// maxEndpointRequest 是一次请求的上限：上下文视图与每步请求相同（含内联的图片与音频）。
const maxEndpointRequest = 32 << 20

const (
	// DefaultEndpointMax 是经端点的调用的合计并发上限（serve -loop-model-concurrency）。须小于受限的提供商并发：
	// 端点的调用与 Run 的交互调用同类准入，没有这个上限，一个外部服务凭一张令牌就能占满提供商、让其他业务线排队。
	DefaultEndpointMax = 8
	// stepMax 是同一步（同一张令牌）同时进行的调用上限：框架在一步内通常顺序调用模型，留一点并行的余地。
	stepMax = 2
	// endpointWait 是在并发上限处排队的最长时间，超过返回 503 busy。
	endpointWait = 2 * time.Second
)

// Endpoint 是外部循环调用模型的 OpenAI 兼容端点（ADR-0063）：以步令牌为 API key，只接受 AgentDef 的
// loop.models 中的模型，经 Model（模型网关与准入）调用，照常计量、执行配额。不做内容安全检查：上下文在进入
// 外部服务之前已经检查过，模型的输出只经这一步的回答到达用户与日志，回答在写日志之前检查（第 5 条）。
type Endpoint struct {
	Tracer *tracing.Tracer
	// Keys 按 (业务线, 循环) 取出登记的步令牌密钥与一步的时限（Registry.Keys）。
	Keys   KeyLookup
	Agents interface {
		Get(*v1.AgentRef) (*agentdef.Def, error)
	}
	// Access 非 nil 时，令牌中的 AgentDef 须是业务线的 agents.allow 允许的（ADR-0060）：持有密钥的一方能在令牌里
	// 写任意 AgentDef，不能借此用到业务线不该用的 AgentDef 及其模型。
	Access interface {
		AllowsAgent(businessLine, name string) bool
	}
	// Max 是合计并发上限，0 取 DefaultEndpointMax。
	Max    int
	Slots  model.Slots
	Model  model.Provider
	Quotas *usage.Quotas
	Meter  *usage.Meter
	// Deletions 为 nil 时不查删除记录（评测）。
	Deletions lifecycle.Deletions
	// Clock 是计量与配额的时间；Now 是验证步令牌的时间，nil 时取 time.Now：令牌由 Client 按真实时间签发，
	// 评测拨过的 Clock 会把它判为过期。
	Clock  clock.Clock
	Now    func() time.Time
	IDs    ids.Generator
	Logger *slog.Logger
	// OnUsage 非 nil 时在每次调用返回后得到用量：评测据此把经端点的调用计入 Run 的 token 与缓存命中
	// （评测报告读日志中 AssistantMessage 的用量，外部循环的那一步没有）。
	OnUsage func(c *StepClaims, model string, u *v1.Usage)
}

func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || e.Keys == nil {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "a step token is required")
		return
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	c, err := VerifyStep(token, e.Keys, now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or expired step token")
		return
	}
	span := e.Tracer.Start(tracing.LoopModel, tracing.Session(c.SessionID), tracing.Run(c.RunID), tracing.Attempt(c.Attempt),
		tracing.Count(tracing.Turn, int(c.Turn)))
	outcome := tracing.Error
	defer func() { span.End(outcome) }()
	ctx = span.Context(ctx)
	// 调用不超出这一步的时限：步已经超时，回答不会被采用，继续调用只是浪费。读请求体同样（慢速上传不能拖过令牌的有效期）。
	ctx, cancel := context.WithDeadline(ctx, c.ExpiresAt.Time)
	defer cancel()
	_ = http.NewResponseController(w).SetReadDeadline(c.ExpiresAt.Time)
	if c.Background {
		ctx = model.WithBackground(ctx)
	}
	// 删除之后不得再处理这个 Session 的内容（ADR-0015、ADR-0062 第 7 条）；注销中的 EndUser 同样。
	if gone, err := e.deleted(ctx, c); err != nil || gone {
		if err != nil {
			e.log(ctx, "loop endpoint: deletion check failed", c, err)
			writeError(w, http.StatusServiceUnavailable, "unavailable", "deletion check failed")
			return
		}
		writeError(w, http.StatusNotFound, "session_deleted", "the session has been deleted")
		return
	}
	var req chatRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEndpointRequest+1))
	if err != nil || len(body) > maxEndpointRequest || json.Unmarshal(body, &req) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "unreadable or too large request body")
		return
	}
	def, err := e.Agents.Get(&v1.AgentRef{Name: c.AgentName, Version: c.AgentVersion})
	// AgentDef 须指向令牌所属的循环、且业务线允许它：密钥只能为自己这个循环的 AgentDef 调用模型。
	if err != nil || def.Loop == nil || def.Loop.Server != c.Loop || (e.Access != nil && !e.Access.AllowsAgent(c.BusinessLine, c.AgentName)) {
		writeError(w, http.StatusBadRequest, "model_not_allowed", "the agent does not belong to this loop")
		return
	}
	if !def.Loop.AllowsModel(req.Model) {
		writeError(w, http.StatusBadRequest, "model_not_allowed", "the model is not in the agent's loop.models")
		return
	}
	in, err := req.decode()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if p, err := e.Quotas.Check(ctx, c.BusinessLine, c.EndUser, e.Clock.Now()); err != nil || p != nil {
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "quota check failed")
			return
		}
		// 与提供商限流区分：外部服务不应在这一步内重试，下一步之前平台的检查会让 Run 挂起（ADR-0063 第 4 条）。
		metrics.QuotaRejections.WithLabelValues(p.Scope, "loop_endpoint").Inc()
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "the quota is exhausted; do not retry within this step")
		return
	}
	maxCalls := e.Max
	if maxCalls <= 0 {
		maxCalls = DefaultEndpointMax
	}
	release, err := e.Slots.AcquireAll(ctx, []model.SlotReq{
		{Key: "loop:total", Limit: maxCalls, Wait: endpointWait},
		{Key: "loop:step:" + c.SessionID + "/" + c.RunID + "/" + fmt.Sprint(c.Attempt, "/", c.Turn), Limit: stepMax, Wait: endpointWait},
	})
	if err != nil {
		outcome = model.TraceOutcome(err)
		status, code := modelStatus(ctx, err)
		writeError(w, status, code, "the loop endpoint is busy")
		return
	}
	defer release()
	out := &responder{w: w, model: req.Model, id: "chatcmpl-" + e.IDs(), created: e.Clock.Now().Unix(), stream: req.Stream,
		usage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage}
	var onDelta func(model.Delta)
	if req.Stream {
		onDelta = out.delta
	}
	resp, callErr := e.Model.Generate(ctx, in, onDelta)
	if resp != nil {
		// token 在返回时已消耗：外部服务断开不撤销计量（ADR-0019）。
		meterCtx, cancelMeter := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		e.Meter.Model(meterCtx, usage.Scope{SessionID: c.SessionID, BusinessLine: c.BusinessLine, EndUser: c.EndUser},
			"use_"+e.IDs(), agentdef.Label(&v1.AgentRef{Name: c.AgentName, Version: c.AgentVersion}), req.Model, resp.Usage, e.Clock.Now())
		cancelMeter()
		metrics.ModelTokens.WithLabelValues("input").Add(float64(resp.Usage.GetInputTokens()))
		metrics.ModelTokens.WithLabelValues("output").Add(float64(resp.Usage.GetOutputTokens()))
		metrics.ModelTokens.WithLabelValues("cached_input").Add(float64(resp.Usage.GetCachedInputTokens()))
		if e.OnUsage != nil {
			e.OnUsage(c, req.Model, resp.Usage)
		}
	}
	if callErr != nil || resp == nil {
		if callErr != nil {
			outcome = model.TraceOutcome(callErr)
		}
		// 错误不回给外部服务原文，但要能排查：ProviderError 只含模型、状态码、提供商的错误码与请求 ID（日志只记 ID）。
		e.log(ctx, "loop endpoint: model call failed", c, callErr)
		status, code := modelStatus(ctx, callErr)
		out.fail(status, code)
		return
	}
	// 调用期间 Session 可能被删除：不再把输出交给外部服务（流式时已发出的增量收不回，以错误结束流）。
	if gone, err := e.deleted(context.WithoutCancel(ctx), c); err != nil || gone {
		if err != nil {
			e.log(ctx, "loop endpoint: deletion check failed", c, err)
			out.fail(http.StatusServiceUnavailable, "unavailable")
			return
		}
		out.fail(http.StatusNotFound, "session_deleted")
		return
	}
	out.finish(resp)
	outcome = tracing.OK
}

func (e *Endpoint) deleted(ctx context.Context, c *StepClaims) (bool, error) {
	if e.Deletions == nil {
		return false, nil
	}
	gone, err := lifecycle.Deleted(ctx, e.Deletions, c.SessionID)
	if err != nil || gone {
		return gone, err
	}
	return lifecycle.EndUserDeleted(ctx, e.Deletions, c.BusinessLine, c.EndUser)
}

func (e *Endpoint) log(ctx context.Context, msg string, c *StepClaims, err error) {
	if e.Logger != nil {
		e.Logger.WarnContext(ctx, msg, "session", c.SessionID, "run", c.RunID, "err", err)
	}
}

// modelStatus 把调用错误映射为外部服务看得懂的状态与错误码，不带提供商的错误原文（可能回显内容）。
func modelStatus(ctx context.Context, err error) (int, string) {
	switch {
	case errors.Is(err, model.ErrContextOverflow):
		return http.StatusBadRequest, "context_length_exceeded"
	case errors.Is(err, model.ErrBusy):
		return http.StatusServiceUnavailable, "busy"
	case ctx.Err() != nil, errors.Is(err, model.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	}
	var pe *model.ProviderError
	if errors.As(err, &pe) && pe.Status == http.StatusTooManyRequests {
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	}
	return http.StatusBadGateway, "provider_error"
}

// chatRequest 是 Chat Completions 请求中端点认得的字段；其余（temperature 等采样参数）忽略，由 AgentDef 与提供商决定。
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Tools         []chatTool    `json:"tools"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens           int `json:"max_tokens"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
	N                   int `json:"n"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
}

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// decode 把请求转成网关的请求。错误信息只描述结构，不引用内容（错误会回给外部服务，也可能进入日志）。
func (r *chatRequest) decode() (*model.Request, error) {
	if r.N > 1 {
		return nil, errors.New("n > 1 is not supported")
	}
	out := &model.Request{Model: r.Model, MaxTokens: r.MaxCompletionTokens}
	if out.MaxTokens == 0 {
		out.MaxTokens = r.MaxTokens
	}
	var system []string
	for i, m := range r.Messages {
		blocks, err := contentBlocks(m.Content, m.Role == "user")
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, model.Text(blocks))
		case "user":
			out.Messages = append(out.Messages, model.Message{Role: model.RoleUser, Content: blocks})
		case "assistant":
			msg := model.Message{Role: model.RoleAssistant, Content: blocks}
			for _, c := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, &v1.ToolCall{CallId: c.ID, Capability: c.Function.Name, ArgumentsJson: c.Function.Arguments})
			}
			out.Messages = append(out.Messages, msg)
		case "tool":
			if m.ToolCallID == "" {
				return nil, fmt.Errorf("messages[%d]: tool_call_id is required", i)
			}
			out.Messages = append(out.Messages, model.Message{Role: model.RoleTool, Content: blocks, ToolCallID: m.ToolCallID})
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role", i)
		}
	}
	out.System = strings.Join(system, "\n\n")
	for i, t := range r.Tools {
		if t.Type != "function" || t.Function.Name == "" {
			return nil, fmt.Errorf("tools[%d]: only named function tools are supported", i)
		}
		out.Tools = append(out.Tools, model.ToolSpec{Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters})
	}
	return out, nil
}

// contentBlocks 解析消息内容：字符串，或 text、image_url、input_audio 片段。媒体只在用户消息中、且只接受内联数据
// （data: URL 与 base64）：内核不替外部服务转发远程地址（ADR-0001），每步请求中的媒体本来就是内联的。
func contentBlocks(raw json.RawMessage, allowMedia bool) ([]*v1.ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return model.TextBlocks(text), nil
	}
	var parts []chatPart
	if json.Unmarshal(raw, &parts) != nil {
		return nil, errors.New("content must be a string or an array of parts")
	}
	var out []*v1.ContentBlock
	for _, p := range parts {
		switch {
		case p.Type == "text":
			out = append(out, model.TextBlocks(p.Text)...)
		case p.Type == "image_url" && allowMedia && p.ImageURL != nil:
			mt, data, ok := dataURL(p.ImageURL.URL)
			if !ok || !imageTypes[mt] {
				return nil, errors.New("image_url must be a base64 data: URL of a png, jpeg, webp or gif image")
			}
			out = append(out, &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: mt, Data: data}}})
		case p.Type == "input_audio" && allowMedia && p.InputAudio != nil:
			data, err := base64.StdEncoding.DecodeString(p.InputAudio.Data)
			mt := audioTypes[p.InputAudio.Format]
			if err != nil || mt == "" {
				return nil, errors.New("input_audio must carry base64 data in wav, mp3 or m4a")
			}
			out = append(out, &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: mt, Data: data}}})
		default:
			return nil, errors.New("unsupported content part")
		}
	}
	return out, nil
}

// 媒体类型只收提供商接口支持的（与 internal/model/openaicompat 发出的一致）。
var (
	imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}
	audioTypes = map[string]string{"wav": "audio/wav", "mp3": "audio/mpeg", "m4a": "audio/mp4"}
)

func dataURL(u string) (mimeType string, data []byte, ok bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", nil, false
	}
	meta, enc, ok := strings.Cut(rest, ",")
	mimeType, isBase64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isBase64 {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(enc)
	return mimeType, data, err == nil
}

// responder 写回答：非流式为一个 chat.completion，流式为 SSE 的 chat.completion.chunk，以 [DONE] 结束。
type responder struct {
	w       http.ResponseWriter
	model   string
	id      string
	created int64
	stream  bool
	usage   bool
	started bool
	// text 与 reasoning 是已经以增量发出的内容：提供商不流式输出（或只输出了一部分）时，结束前补发其余部分。
	text, reasoning string
}

type outMessage struct {
	Role             string         `json:"role,omitempty"`
	Content          *string        `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

type outChoice struct {
	Index        int         `json:"index"`
	Message      *outMessage `json:"message,omitempty"`
	Delta        *outMessage `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

type outUsage struct {
	PromptTokens        uint64 `json:"prompt_tokens"`
	CompletionTokens    uint64 `json:"completion_tokens"`
	TotalTokens         uint64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens uint64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type outResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []outChoice `json:"choices"`
	Usage   *outUsage   `json:"usage,omitempty"`
}

func (o *responder) chunk(choices []outChoice, u *outUsage) {
	if !o.started {
		o.started = true
		o.w.Header().Set("Content-Type", "text/event-stream")
		o.w.Header().Set("Cache-Control", "no-cache")
		o.w.WriteHeader(http.StatusOK)
	}
	b, _ := json.Marshal(outResponse{ID: o.id, Object: "chat.completion.chunk", Created: o.created, Model: o.model, Choices: choices, Usage: u})
	fmt.Fprintf(o.w, "data: %s\n\n", b)
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responder) delta(d model.Delta) {
	o.text += d.Text
	o.reasoning += d.Reasoning
	m := &outMessage{ReasoningContent: d.Reasoning}
	if !o.started {
		m.Role = "assistant"
	}
	if d.Text != "" {
		m.Content = &d.Text
	}
	o.chunk([]outChoice{{Delta: m}}, nil)
}

func (o *responder) finish(resp *model.Response) {
	reason := resp.FinishReason
	if reason == "" {
		reason = "stop"
		if len(resp.ToolCalls) > 0 {
			reason = "tool_calls"
		}
	}
	var calls []chatToolCall
	for i, c := range resp.ToolCalls {
		tc := chatToolCall{ID: c.GetCallId(), Type: "function"}
		tc.Function.Name, tc.Function.Arguments = c.GetCapability(), c.GetArgumentsJson()
		if o.stream {
			tc.Index = &i
		}
		calls = append(calls, tc)
	}
	u := &outUsage{PromptTokens: resp.Usage.GetInputTokens(), CompletionTokens: resp.Usage.GetOutputTokens(),
		TotalTokens: resp.Usage.GetInputTokens() + resp.Usage.GetOutputTokens()}
	u.PromptTokensDetails.CachedTokens = resp.Usage.GetCachedInputTokens()
	if !o.stream {
		m := &outMessage{Role: "assistant", ReasoningContent: resp.Reasoning, ToolCalls: calls}
		if text := model.Text(resp.Content); text != "" || len(calls) == 0 {
			m.Content = &text
		}
		o.w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(o.w).Encode(outResponse{ID: o.id, Object: "chat.completion", Created: o.created, Model: o.model,
			Choices: []outChoice{{Message: m, FinishReason: &reason}}, Usage: u})
		return
	}
	if rest, rr := remainder(model.Text(resp.Content), o.text), remainder(resp.Reasoning, o.reasoning); rest != "" || rr != "" {
		m := &outMessage{ReasoningContent: rr}
		if rest != "" {
			m.Content = &rest
		}
		if !o.started {
			m.Role = "assistant"
		}
		o.chunk([]outChoice{{Delta: m}}, nil)
	}
	if len(calls) > 0 {
		m := &outMessage{ToolCalls: calls}
		if !o.started {
			m.Role = "assistant"
		}
		o.chunk([]outChoice{{Delta: m}}, nil)
	}
	o.chunk([]outChoice{{Delta: &outMessage{}, FinishReason: &reason}}, nil)
	if o.usage {
		o.chunk([]outChoice{}, u)
	}
	fmt.Fprint(o.w, "data: [DONE]\n\n")
}

// remainder 是完整内容中尚未以增量发出的部分。增量与完整内容对不上时（提供商的异常）补发全部：
// 客户端拼出的文字可能重复，但不会少。
func remainder(full, sent string) string {
	if rest, ok := strings.CutPrefix(full, sent); ok {
		return rest
	}
	return full
}

// fail 报告调用失败。流已经开始时状态码已发出，以一个错误事件结束流。
func (o *responder) fail(status int, code string) {
	if !o.started {
		writeError(o.w, status, code, "the model call failed")
		return
	}
	b, _ := json.Marshal(errorBody(code, "the model call failed"))
	fmt.Fprintf(o.w, "data: %s\n\n", b)
}

func errorBody(code, msg string) map[string]any {
	return map[string]any{"error": map[string]string{"message": msg, "type": code, "code": code}}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody(code, msg))
}
