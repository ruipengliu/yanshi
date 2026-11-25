package looptest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/extloop"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// Status 是一个一致性场景的结果状态。
type Status string

const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Result 是一个场景的执行结果。
type Result struct {
	Name   string
	Status Status
	// Reason 在 FAIL 时说明失败原因，在 SKIP 时说明跳过原因，在 PASS 时可附说明（如 streaming）。
	Reason   string
	Duration time.Duration
}

// Config 配置一致性套件的运行。
type Config struct {
	// URL 是被测外部循环服务的 HTTP 地址。
	URL string
	// Headers 随每个请求发给被测服务。
	Headers map[string]string
	// Timeout 是每一步请求的时限，<=0 时默认 extloop.DefaultTimeout。
	Timeout time.Duration
	// Model 是套件托管的假模型，调用方须先让被测服务指向它。
	Model *Model
	// Restart 可选：重启被测服务进程；为 nil 时 restart 场景记为跳过。
	Restart func(context.Context) error
	// HTTP 可选：用于请求被测服务的 HTTP 客户端；为 nil 时使用不跟随重定向的默认客户端。
	HTTP *http.Client
	// Run 可选：只运行列出的场景名；为空时运行全部场景。
	Run []string
}

var requestMarshal = protojson.MarshalOptions{EmitUnpopulated: true}

type scenario struct {
	name string
	run  func(context.Context, *runner) (Status, string)
}

// ScenarioNames 返回全部场景的固定名称（按执行顺序）。
func ScenarioNames() []string {
	out := make([]string, len(allScenarios))
	for i, s := range allScenarios {
		out[i] = s.name
	}
	return out
}

// Run 按配置运行一致性场景并返回逐项结果。
func Run(ctx context.Context, cfg Config) []Result {
	if cfg.URL == "" {
		return []Result{{Name: "config", Status: StatusFail, Reason: "Config.URL is required"}}
	}
	if cfg.Model == nil {
		return []Result{{Name: "config", Status: StatusFail, Reason: "Config.Model is required"}}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = extloop.DefaultTimeout
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	selected, unknown := selectScenarios(cfg.Run)
	var results []Result
	for _, u := range unknown {
		results = append(results, Result{Name: u, Status: StatusFail, Reason: "unknown scenario name"})
	}

	r := &runner{cfg: cfg, hc: hc}
	for _, sc := range selected {
		start := time.Now()
		status, reason := sc.run(ctx, r)
		results = append(results, Result{
			Name:     sc.name,
			Status:   status,
			Reason:   reason,
			Duration: time.Since(start),
		})
	}
	return results
}

// RunT 是供 go test 使用的包装：每个场景作为独立子测试运行。
func RunT(t *testing.T, cfg Config) {
	t.Helper()
	for _, res := range Run(t.Context(), cfg) {
		t.Run(res.Name, func(t *testing.T) {
			switch res.Status {
			case StatusSkip:
				t.Skip(res.Reason)
			case StatusFail:
				t.Fatal(res.Reason)
			}
		})
	}
}

func selectScenarios(filter []string) ([]scenario, []string) {
	if len(filter) == 0 {
		return allScenarios, nil
	}
	want := map[string]bool{}
	for _, f := range filter {
		if f = strings.TrimSpace(f); f != "" {
			want[f] = true
		}
	}
	var out []scenario
	matched := map[string]bool{}
	for _, s := range allScenarios {
		if want[s.name] {
			out = append(out, s)
			matched[s.name] = true
		}
	}
	var unknown []string
	for _, f := range filter {
		f = strings.TrimSpace(f)
		if f != "" && !matched[f] && !slices.Contains(unknown, f) {
			unknown = append(unknown, f)
		}
	}
	return out, unknown
}

type runner struct {
	cfg Config
	hc  *http.Client
	seq int
}

func (r *runner) nextID(prefix, scenario string) string {
	r.seq++
	return fmt.Sprintf("lt_%s_%s_%d", prefix, scenario, r.seq)
}

type stepOutcome struct {
	resp     *v1.LoopStepResponse
	streamed bool
	deltas   int
}

// sendStep 发出一次 LoopStepRequest 并执行全部通用契约校验：
// 状态码 200、extloop.ReadResponse 解析、工具白名单、纯文本回复、state 上限、时限与系统指令转发检查。
func (r *runner) sendStep(ctx context.Context, req *v1.LoopStepRequest) (*stepOutcome, error) {
	body, err := marshalWithUnknownFields(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(sctx, http.MethodPost, r.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range r.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/x-ndjson, application/json")

	resp, err := r.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d, want 200", resp.StatusCode)
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	streamed := mt == "application/x-ndjson"

	deltas := 0
	out, err := extloop.ReadResponse(resp, func(model.Delta) { deltas++ })
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if err := checkStepResponse(out, req); err != nil {
		return nil, err
	}
	return &stepOutcome{resp: out, streamed: streamed, deltas: deltas}, nil
}

// sendRaw 发送一步请求但不做通用契约校验，供 model-failure 场景检查服务是否给出了回答：返回状态码，
// 状态码为 200 时另返回能否解析出一个回答（流式缺少最后一行、无法解析都算没有回答，平台按失败处理，ADR-0062 第 4 条）。
func (r *runner) sendRaw(ctx context.Context, req *v1.LoopStepRequest) (status int, answered bool, err error) {
	body, err := marshalWithUnknownFields(req)
	if err != nil {
		return 0, false, fmt.Errorf("marshal request: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(sctx, http.MethodPost, r.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, false, fmt.Errorf("build request: %w", err)
	}
	for k, v := range r.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/x-ndjson, application/json")

	resp, err := r.hc.Do(hreq)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, false, nil
	}
	_, rerr := extloop.ReadResponse(resp, nil)
	return resp.StatusCode, rerr == nil, nil
}

// marshalWithUnknownFields 按平台规则（EmitUnpopulated）序列化请求，并在顶层与嵌套消息（agent）
// 中各附一个未知字段，验证外部服务按向后兼容约定忽略未知字段。
func marshalWithUnknownFields(req *v1.LoopStepRequest) ([]byte, error) {
	raw, err := requestMarshal.Marshal(req)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return nil, err
	}
	top["looptestUnknownTop"] = "ignore"
	if agent, ok := top["agent"].(map[string]any); ok {
		agent["looptestUnknownNested"] = 1
	}
	return json.Marshal(top)
}

// checkStepResponse 执行与平台 runtime.checkReply（internal/runtime/loop.go）一致的回答校验，两处须同步修改。
func checkStepResponse(out *v1.LoopStepResponse, req *v1.LoopStepRequest) error {
	if out == nil {
		return fmt.Errorf("empty response")
	}
	if len(out.GetState()) > session.MaxLoopState {
		return fmt.Errorf("state size %d exceeds limit %d", len(out.GetState()), session.MaxLoopState)
	}
	allowed := make(map[string]bool, len(req.GetTools()))
	for _, t := range req.GetTools() {
		allowed[t.GetName()] = true
	}
	for i, tc := range out.GetToolCalls() {
		if !allowed[tc.GetCapability()] {
			return fmt.Errorf("tool_calls[%d] capability %q is not in request tools", i, tc.GetCapability())
		}
	}
	for i, b := range out.GetContent() {
		if b.GetText() == nil {
			return fmt.Errorf("content[%d] is not a text block", i)
		}
	}
	return nil
}

func (r *runner) checkViolations(scenario string) error {
	vs := r.cfg.Model.Violations(scenario)
	if len(vs) > 0 {
		return fmt.Errorf("%s", strings.Join(vs, "; "))
	}
	return nil
}
