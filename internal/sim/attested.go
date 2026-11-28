package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
)

var simAttestedName = mcpcap.ToolName("sim", "pages")

// 用同步内存 HTTP 传输替代网络，仍经真实 Connector 校验登记与页元数据。
// 随机页只在 tools/call 请求内生成，Worker 等待响应后才推进下一步，不引入后台随机动作。
func newSimAttestedConnector(w *World) *mcpcap.Connector {
	return &mcpcap.Connector{
		Servers:    []mcpcap.Server{{Name: "sim", BusinessLine: "bl", URL: "http://sim.invalid/mcp", Timeout: time.Minute, AttestsSpeakers: true}},
		HTTPClient: &http.Client{Transport: simPageTransport{w}},
		// 工具列表固定，刷新不消耗随机数；较长缓存减少重复发现。
		CacheTTL: 24 * time.Hour,
	}
}

type simPageTransport struct{ w *World }

func (t simPageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	response := func(status int, body []byte) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req}
	}
	if req.Method == http.MethodDelete {
		return response(http.StatusOK, nil), nil
	}
	if req.Method != http.MethodPost {
		return response(http.StatusMethodNotAllowed, nil), nil
	}
	defer req.Body.Close()
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			Name            string                     `json:"name"`
			Meta            map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.NewDecoder(req.Body).Decode(&rpc); err != nil {
		return nil, err
	}
	if len(rpc.ID) == 0 {
		return response(http.StatusAccepted, nil), nil
	}
	var result any
	switch rpc.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": rpc.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "sim", "version": "1"}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "pages", "inputSchema": map[string]any{"type": "object"},
			"annotations": map[string]any{"readOnlyHint": true, "idempotentHint": true}}}}
	case "tools/call":
		if rpc.Params.Name != "pages" || req.Header.Get("X-Yanshi-Business-Line") != "bl" || len(rpc.Params.Meta["yanshi/page_options"]) == 0 {
			return nil, fmt.Errorf("unexpected simulated MCP call")
		}
		result = t.w.attestedPageResult()
	default:
		return nil, fmt.Errorf("unexpected simulated MCP method %s", rpc.Method)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	if err != nil {
		return nil, err
	}
	return response(http.StatusOK, body), nil
}

func (w *World) attestedPageResult() map[string]any {
	p := capture.StructuredPage{Kind: "page", Span: "57.0", Done: true}
	for i, g := range w.captureSegments() {
		who := "wearer"
		if g.GetSpeaker() == v1.Speaker_SPEAKER_OTHER {
			who = "other"
		}
		p.Segments = append(p.Segments, capture.PageSegment{Ref: fmt.Sprintf("57.%d", i), Speaker: who, SpeakerKey: g.GetSpeakerKey(), SpeakerRef: g.GetSpeakerRef(), Text: g.GetText(), SpokenAt: g.GetSpokenAt().AsTime().Format(time.RFC3339), TimeZone: g.GetTimeZone()})
	}
	failed := w.faults && w.chance(0.1)
	// 无效页一半是错误的说话人引用：5% 时这条路径在 300 个种子中只出现几次。
	if w.faults && w.chance(0.15) {
		if w.chance(0.5) {
			p.Kind = "invalid"
		} else {
			p.Segments[1].SpeakerRef = "invalid ref"
		}
	}
	return map[string]any{"content": []any{}, "isError": failed, "_meta": map[string]any{"yanshi/page": p}}
}

// 只根据模型实际收到的页触发 Memory 预检查，避免在页尚未写入或已被拦截时虚报覆盖。
func attestedMemoryPrecheck(messages []model.Message) bool {
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	if last.Role != model.RoleTool {
		return false
	}
	page := false
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.GetCallId() == last.ToolCallID && call.GetCapability() == simAttestedName {
				page = true
			}
		}
	}
	if !page {
		return false
	}
	// Transcript 已把结构化片段展开为模型可见文本；拦截占位在右括号后没有空格。
	// 这里只驱动脚本化模型，真正的来源与闸门仍由日志中的结构化说话人决定。
	text := model.Text(last.Content)
	for _, key := range []string{"other-1", "other-2"} {
		if strings.Contains(text, "[他人 "+key+"] ") || strings.Contains(text, "[他人 "+key+" @P-0012] ") {
			return true
		}
	}
	return false
}

var captureWords = []string{"报价", "周五", "张三", "合同", "预算", "会议", "好的", "下周", "确认", "发邮件"}

// captureSegments 每页包含佩戴者与他人，额外片段的说话人随机；保证来源闸门持续得到两种输入。
func (w *World) captureSegments() []*v1.CaptureSegment {
	segs := make([]*v1.CaptureSegment, 2+w.rng.IntN(3))
	for i := range segs {
		text := fmt.Sprintf("%s%s %d", captureWords[w.rng.IntN(len(captureWords))], captureWords[w.rng.IntN(len(captureWords))], w.rng.IntN(1000))
		if i == 0 || (i > 1 && w.chance(0.5)) {
			segs[i] = &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: text}
		} else {
			segs[i] = &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: fmt.Sprintf("other-%d", 1+w.rng.IntN(2)), SpeakerRef: "P-0012", Text: text}
		}
		// 带时间才有时段与续页；同批的片段属于同一时段，时间来自模拟时钟。
		segs[i].SpokenAt = timestamppb.New(w.clock.Now())
		segs[i].TimeZone = "UTC"
	}
	return segs
}

// checkAttested 检查日志中的一批旁听转写没有本应拦截的内容（ADR-0021）。送检的单位是整批，与一条输入相同：整批没有拦截时，
// 整批的送检文本不违规；有拦截时，保留下来的每一段单独看都不违规。与 Worker 的整页送检文字一致。
func (w *World) checkAttested(sid string, e *v1.Event) error {
	var segs []*v1.CaptureSegment
	if r := e.GetToolResult(); r.GetAttested() {
		for _, b := range r.GetContent() {
			if g := b.GetCapture(); g != nil {
				segs = append(segs, g)
			}
		}
	}
	if len(segs) == 0 {
		return nil
	}
	redacted := slices.ContainsFunc(segs, (*v1.CaptureSegment).GetRedacted)
	texts := make([]string, len(segs))
	for i, g := range segs {
		texts[i] = g.GetText()
	}
	if !redacted && w.blocked(strings.Join(texts, "\n")) {
		return fmt.Errorf("invariant: blocked capture reached the log of %s at seq %d", sid, e.GetSeq())
	}
	for _, g := range segs {
		if redacted && !g.GetRedacted() && w.blocked(g.GetText()) {
			return fmt.Errorf("invariant: blocked capture segment reached the log of %s at seq %d", sid, e.GetSeq())
		}
	}
	return nil
}

// countAttestedResults 只计入已写入日志的结果；生成参数、随后被打断或拒绝的调用不能冒充成功读取。
func (w *World) countAttestedResults(events []*v1.Event) {
	calls := map[string]*v1.ToolCall{}
	for _, e := range events {
		if m := e.GetAssistantMessage(); m != nil {
			for _, c := range m.GetToolCalls() {
				calls[m.GetRunId()+"/"+c.GetCallId()] = c
			}
		}
		if r := e.GetToolResult(); r != nil {
			if r.GetAttested() {
				w.Stats.AttestedResults++
				for _, b := range r.GetContent() {
					if b.GetCapture().GetSpeakerRef() != "" {
						w.Stats.AttestedSpeakerRefs++
					}
					if b.GetCapture().GetRedacted() {
						w.Stats.AttestedRedactions++
					}
				}
			}
			if r.GetIsError() && calls[r.GetRunId()+"/"+r.GetCallId()].GetCapability() == simAttestedName {
				w.Stats.AttestedErrors++
				if model.Text(r.GetContent()) == "业务线返回的旁听页无效：说话人引用" {
					w.Stats.AttestedSpeakerRefErrors++
				}
			}

		}
	}
}

type simAttestedSource struct{ w *World }

func (s simAttestedSource) Tools(ctx context.Context, businessLine string, patterns []string) ([]capability.Tool, error) {
	if !s.w.opts.Attested {
		return nil, nil
	}
	return s.w.attested.Tools(ctx, businessLine, patterns)
}
