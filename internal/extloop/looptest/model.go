// Package looptest 是外部循环的一致性套件（ADR-0062 第 8 条）：扮演平台经 HTTP 驱动被测外部循环服务，
// 同时托管一个脚本化的 OpenAI 兼容假模型，以验证"框架把什么交给了模型"与"服务按契约回答了什么"。
package looptest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// markerPrefix 是套件写入请求 system 的场景标记前缀：假模型据此识别请求属于哪个场景，
// 若模型收到的系统消息中没有该标记，说明被测服务丢弃了平台的系统指令。
const markerPrefix = "looptest:"

var markerRE = regexp.MustCompile(`looptest:([a-z0-9-]+)`)

// ModelCall 是假模型收到的一次 Chat Completions 请求（规范化后的视图）。
type ModelCall struct {
	Scenario string
	System   string
	Messages []ModelMessage
	Tools    []ModelTool
	Stream   bool
}

// ModelMessage 是假模型收到的一条消息。
type ModelMessage struct {
	Role       string
	Content    string
	ToolCalls  []ModelToolCall
	ToolCallID string
}

// ModelToolCall 是消息或回答中的一次工具调用。
type ModelToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ModelTool 是假模型收到的一个工具定义。
type ModelTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ModelReply 是场景脚本给假模型的回答。Status 非 0（如 500）时假模型返回该 HTTP 错误状态码。
type ModelReply struct {
	Text      string
	Reasoning string
	ToolCalls []ModelToolCall
	Status    int
}

// ScriptFunc 根据假模型本次看到的请求决定回答。
type ScriptFunc func(call ModelCall) ModelReply

// Model 是一个脚本化的 OpenAI 兼容假模型（http.Handler），支持 POST /v1/chat/completions 与
// POST /chat/completions 的非流式 JSON 与流式 SSE 回答。并发安全。
type Model struct {
	mu         sync.Mutex
	scripts    map[string]ScriptFunc
	calls      map[string][]ModelCall
	violations map[string][]string
	active     string
	seq        int
	// tokens 是套件在各场景中签发的步令牌；withTokens 时模型调用必须以其中之一为 API key（ADR-0063 第 6 条）。
	tokens     map[string]map[string]bool
	withTokens bool
}

// NewModel 构造一个新的假模型。
func NewModel() *Model {
	return &Model{
		scripts:    map[string]ScriptFunc{},
		calls:      map[string][]ModelCall{},
		violations: map[string][]string{},
		tokens:     map[string]map[string]bool{},
	}
}

func (m *Model) requireTokens(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.withTokens = on
}

func (m *Model) issueToken(scenario, tok string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens[scenario] == nil {
		m.tokens[scenario] = map[string]bool{}
	}
	m.tokens[scenario][tok] = true
}

// beginScenario 为一个场景注册脚本、清空该场景的历史记录并置为当前活跃场景。
func (m *Model) beginScenario(name string, fn ScriptFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts[name] = fn
	delete(m.calls, name)
	delete(m.violations, name)
	delete(m.tokens, name)
	m.active = name
}

// endScenario 清除当前活跃场景标记。
func (m *Model) endScenario(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == name {
		m.active = ""
	}
}

// Calls 返回指定场景在假模型上收到的全部调用副本。
func (m *Model) Calls(scenario string) []ModelCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.calls[scenario]
	out := make([]ModelCall, len(src))
	copy(out, src)
	return out
}

// Violations 返回指定场景记录的模型侧违规（例如未转发平台系统指令）。
func (m *Model) Violations(scenario string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.violations[scenario]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// ServeHTTP 处理 OpenAI 兼容的 Chat Completions 请求。
func (m *Model) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeModelError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var raw chatRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		writeModelError(w, http.StatusBadRequest, "invalid json request")
		return
	}
	call, err := parseChatRequest(&raw)
	if err != nil {
		writeModelError(w, http.StatusBadRequest, err.Error())
		return
	}

	modelName := raw.Model
	if modelName == "" {
		modelName = "looptest-model"
	}
	includeUsage := raw.StreamOptions != nil && raw.StreamOptions.IncludeUsage

	m.mu.Lock()
	m.seq++
	callSeq := m.seq
	scenario := extractScenario(call.System)
	if scenario == "" {
		// 平台发出的每个请求都在 system 中带 looptest:<场景名>；模型未收到该标记说明系统指令被剥离。
		target := m.active
		m.violations[target] = append(m.violations[target], "平台的系统指令没有交给模型（系统消息中未见 looptest:<场景名> 标记）")
		m.mu.Unlock()
		writeChatReply(w, callSeq, modelName, raw.Stream, includeUsage, ModelReply{Text: "missing system marker"})
		return
	}
	call.Scenario = scenario
	if bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); m.withTokens && !m.tokens[scenario][bearer] {
		m.violations[scenario] = append(m.violations[scenario], "模型调用没有以这一步的步令牌为 API key（ADR-0063 第 6 条）")
	}
	if m.withTokens && raw.Model != ModelRef {
		m.violations[scenario] = append(m.violations[scenario], "模型调用没有使用请求 models 中的模型（ADR-0063 第 3 条）")
	}
	m.calls[scenario] = append(m.calls[scenario], call)
	fn := m.scripts[scenario]
	m.mu.Unlock()

	if fn == nil {
		writeModelError(w, http.StatusNotFound, "unknown looptest scenario: "+scenario)
		return
	}
	reply := fn(call)
	if reply.Status != 0 && reply.Status != http.StatusOK {
		writeModelError(w, reply.Status, fmt.Sprintf("looptest injected model error (status %d)", reply.Status))
		return
	}
	// 调用 ID 由假模型负责补齐，与真实模型给出自身 ID 的行为一致。
	if len(reply.ToolCalls) > 0 {
		m.mu.Lock()
		for i := range reply.ToolCalls {
			if reply.ToolCalls[i].ID == "" {
				m.seq++
				reply.ToolCalls[i].ID = fmt.Sprintf("fm_%d", m.seq)
			}
			if reply.ToolCalls[i].Arguments == "" {
				reply.ToolCalls[i].Arguments = "{}"
			}
		}
		m.mu.Unlock()
	}
	writeChatReply(w, callSeq, modelName, raw.Stream, includeUsage, reply)
}

func extractScenario(system string) string {
	sub := markerRE.FindStringSubmatch(system)
	if len(sub) < 2 {
		return ""
	}
	return sub[1]
}

// --- OpenAI Chat Completions 线上结构 ---

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Tools         []chatTool    `json:"tools"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function,omitempty"`
}

func parseChatRequest(raw *chatRequest) (ModelCall, error) {
	call := ModelCall{Stream: raw.Stream}
	var sysParts []string
	for _, rm := range raw.Messages {
		text, err := decodeContent(rm.Content)
		if err != nil {
			return ModelCall{}, fmt.Errorf("message content: %w", err)
		}
		// OpenAI 新客户端可能用 developer 角色承载系统指令，与 system 同等对待。
		if rm.Role == "system" || rm.Role == "developer" {
			if text != "" {
				sysParts = append(sysParts, text)
			}
		}
		mm := ModelMessage{
			Role:       rm.Role,
			Content:    text,
			ToolCallID: rm.ToolCallID,
		}
		for _, tc := range rm.ToolCalls {
			mm.ToolCalls = append(mm.ToolCalls, ModelToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		call.Messages = append(call.Messages, mm)
	}
	call.System = strings.Join(sysParts, "\n")
	for _, rt := range raw.Tools {
		if rt.Function != nil {
			call.Tools = append(call.Tools, ModelTool{
				Name:        rt.Function.Name,
				Description: rt.Function.Description,
				Parameters:  rt.Function.Parameters,
			})
		} else if rt.Name != "" {
			call.Tools = append(call.Tools, ModelTool{Name: rt.Name})
		}
	}
	return call, nil
}

// decodeContent 兼容 OpenAI 消息的两种 content 形态：字符串或分块数组（含 null/缺省）。
func decodeContent(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	if trimmed[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return "", err
		}
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n"), nil
	}
	return "", fmt.Errorf("unsupported content JSON %q", trimmed)
}

func writeModelError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "server_error",
			"code":    "internal_error",
		},
	})
}

func writeChatReply(w http.ResponseWriter, seq int, modelName string, stream, includeUsage bool, reply ModelReply) {
	id := fmt.Sprintf("chatcmpl-lt-%d", seq)
	const created = 1700000000
	finishReason := "stop"
	if len(reply.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	usage := map[string]any{
		"prompt_tokens":     10,
		"completion_tokens": 5,
		"total_tokens":      15,
	}

	if !stream {
		msg := map[string]any{"role": "assistant"}
		if reply.Text != "" || len(reply.ToolCalls) == 0 {
			msg["content"] = reply.Text
		} else {
			msg["content"] = nil
		}
		if reply.Reasoning != "" {
			msg["reasoning_content"] = reply.Reasoning
		}
		if len(reply.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(reply.ToolCalls))
			for _, tc := range reply.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			msg["tool_calls"] = tcs
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   modelName,
			"choices": []map[string]any{{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			}},
			"usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(payload map[string]any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	sendDelta := func(delta map[string]any, finish any) {
		send(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
	}

	roleSent := false
	if reply.Reasoning != "" {
		sendDelta(map[string]any{"role": "assistant", "reasoning_content": reply.Reasoning}, nil)
		roleSent = true
	}
	if reply.Text != "" {
		// 按合法 UTF-8 字符边界拆成两段发送，以覆盖流式增量的拼接逻辑。
		p1, p2 := splitHalfUTF8(reply.Text)
		d := map[string]any{"content": p1}
		if !roleSent {
			d["role"] = "assistant"
			roleSent = true
		}
		sendDelta(d, nil)
		if p2 != "" {
			sendDelta(map[string]any{"content": p2}, nil)
		}
	}
	for i, tc := range reply.ToolCalls {
		// 与 OpenAI 真实流式一致：先发 index/id/type/name，再发 arguments 增量。
		first := map[string]any{
			"tool_calls": []map[string]any{{
				"index": i,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": "",
				},
			}},
		}
		if !roleSent {
			first["role"] = "assistant"
			roleSent = true
		}
		sendDelta(first, nil)
		sendDelta(map[string]any{
			"tool_calls": []map[string]any{{
				"index": i,
				"function": map[string]any{
					"arguments": tc.Arguments,
				},
			}},
		}, nil)
	}
	if !roleSent {
		sendDelta(map[string]any{"role": "assistant", "content": ""}, nil)
	}
	sendDelta(map[string]any{}, finishReason)
	if includeUsage {
		send(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []any{},
			"usage":   usage,
		})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func splitHalfUTF8(s string) (string, string) {
	if len(s) < 2 {
		return s, ""
	}
	mid := len(s) / 2
	for mid < len(s) && !utf8.RuneStart(s[mid]) {
		mid++
	}
	return s[:mid], s[mid:]
}
