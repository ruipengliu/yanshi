// Package extloop 接入业务线登记的外部循环（ADR-0041、ADR-0062）：按 loops/*.yaml 的登记，经 HTTP 以 loop.proto 的
// JSON 请求外部服务决定一步。它实现 runtime.Loop；校验回答、改写调用 ID、内容安全与写日志都在 Worker 中。
package extloop

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/runtime"
)

// Server 是一个外部循环的登记（loops/*.yaml）。以 (业务线, 名称) 为键，只属于一条业务线，且只能是该业务线自己运营的
// 服务：请求带着完整的上下文视图（ADR-0062 第 7 条）。
type Server struct {
	Name         string            `yaml:"name"`
	BusinessLine string            `yaml:"business_line"`
	URL          string            `yaml:"url"`
	Headers      map[string]string `yaml:"headers"`
	// Timeout 是一步的时限（含流式输出），默认 60 秒。
	Timeout time.Duration `yaml:"timeout"`
}

type key struct{ businessLine, name string }

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)
	varRE  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// DefaultTimeout 是一步的默认时限。
const DefaultTimeout = 60 * time.Second

// LoadDir 读取 dir 下所有 *.yaml 登记；目录不存在时返回空。请求头中的 ${VAR} 从环境变量注入，凭证不以明文出现在登记中。
func LoadDir(dir string) ([]Server, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Server
	seen := map[key]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		s, err := parse(b, os.Getenv)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		k := key{s.BusinessLine, s.Name}
		if seen[k] {
			return nil, fmt.Errorf("%s: loop %q registered twice for business line %q", f, s.Name, s.BusinessLine)
		}
		seen[k] = true
		out = append(out, s)
	}
	return out, nil
}

func parse(b []byte, getenv func(string) string) (Server, error) {
	var s Server
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Server{}, err
	}
	if !nameRE.MatchString(s.Name) {
		return Server{}, fmt.Errorf("name %q must match %s", s.Name, nameRE)
	}
	if s.BusinessLine == "" || !(strings.HasPrefix(s.URL, "https://") || strings.HasPrefix(s.URL, "http://")) {
		return Server{}, errors.New("business_line and an http(s) url are required")
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	for k, v := range s.Headers {
		var missing []string
		s.Headers[k] = varRE.ReplaceAllStringFunc(v, func(m string) string {
			val := getenv(varRE.FindStringSubmatch(m)[1])
			if val == "" {
				missing = append(missing, m)
			}
			return val
		})
		if len(missing) > 0 {
			return Server{}, fmt.Errorf("header %s: environment variables %v are not set", k, missing)
		}
	}
	return s, nil
}

// Registry 按 (业务线, 名称) 解析外部循环（runtime.Loops）。
type Registry struct {
	clients map[key]*Client
}

// NewRegistry 为每个登记建一个客户端。hc 为 nil 时使用不跟随重定向的默认客户端：外部地址只来自显式的登记（ADR-0001）。
func NewRegistry(servers []Server, hc *http.Client) *Registry {
	if hc == nil {
		hc = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	r := &Registry{clients: map[key]*Client{}}
	for _, s := range servers {
		if s.Timeout <= 0 {
			s.Timeout = DefaultTimeout
		}
		r.clients[key{s.BusinessLine, s.Name}] = &Client{Server: s, HTTP: hc}
	}
	return r
}

func (r *Registry) Loop(businessLine, name string) (runtime.Loop, error) {
	c, ok := r.clients[key{businessLine, name}]
	if !ok {
		return nil, runtime.ErrLoopNotRegistered
	}
	return c, nil
}

// Servers 返回全部登记，供启动时的价格检查使用。
func (r *Registry) Servers() []Server {
	out := make([]Server, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, c.Server)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].BusinessLine+"/"+out[i].Name < out[j].BusinessLine+"/"+out[j].Name
	})
	return out
}

// Client 请求一个外部循环。
type Client struct {
	Server Server
	HTTP   *http.Client
}

// maxResponse 是一步回答（含流式的全部行）的上限：64 KiB 状态经 base64 约 88 KB，回复与调用参数另计。
const maxResponse = 4 << 20

const ndjson = "application/x-ndjson"

// errUnparsable 不带解析器的错误信息：它会引用出错处的内容，而错误会进入日志与 RunFailed 的原因（日志只记 ID）。
var errUnparsable = errors.New("unparsable response")

var (
	// 输出零值字段：不用 proto 库的外部服务也能直接取到 index 0、is_error false 等。
	marshal   = protojson.MarshalOptions{EmitUnpopulated: true}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// Decide 请求外部服务决定一步。连接层不重试：已输出过增量的步重试会让订阅者看到两份，失败交给 Worker
// 下一步重新请求（ADR-0056 第 6 条）。错误信息只含状态码与结构错误，不含回答内容（日志只记 ID）。
func (c *Client) Decide(ctx context.Context, step runtime.LoopStep, onDelta func(model.Delta)) (*runtime.LoopReply, error) {
	body, err := marshal.Marshal(Request(step))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Server.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range c.Server.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", ndjson+", application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// 不带 *url.Error：它含登记的地址，错误会进入客户端可见的 RunFailed 原因。保留取消的原因
		// （Worker 据此识别被取代的 Attempt）与超时。
		if cause := context.Cause(ctx); cause != nil {
			return nil, fmt.Errorf("loop %s: %w", c.Server.Name, cause)
		}
		return nil, fmt.Errorf("loop %s: request failed", c.Server.Name)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loop %s: status %d", c.Server.Name, resp.StatusCode)
	}
	var out *v1.LoopStepResponse
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == ndjson {
		out, err = readStream(&capped{r: resp.Body, n: maxResponse}, onDelta)
	} else {
		var b []byte
		if b, err = io.ReadAll(io.LimitReader(resp.Body, maxResponse+1)); err == nil {
			if len(b) > maxResponse {
				err = errTooLarge
			} else {
				out = &v1.LoopStepResponse{}
				if unmarshal.Unmarshal(b, out) != nil {
					err = errUnparsable
				}
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("loop %s: %w", c.Server.Name, err)
	}
	return Reply(out), nil
}

// capped 读到超过 n 字节时报错，而不是像 io.LimitReader 那样静默截断：截断处恰好是一行的结尾时，后面的内容会被漏看。
type capped struct {
	r io.Reader
	n int
}

func (c *capped) Read(p []byte) (int, error) {
	if c.n < 0 {
		return 0, errTooLarge
	}
	if len(p) > c.n+1 {
		p = p[:c.n+1]
	}
	k, err := c.r.Read(p)
	c.n -= k
	if c.n < 0 {
		return 0, errTooLarge
	}
	return k, err
}

var errTooLarge = errors.New("response too large")

// readStream 读取 NDJSON：增量行交给 onDelta，最后一行必须是完整的回答（ADR-0056 Q7）。
func readStream(r io.Reader, onDelta func(model.Delta)) (*v1.LoopStepResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxResponse)
	var out *v1.LoopStepResponse
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if out != nil {
			return nil, errors.New("stream continues after the response line")
		}
		var l v1.LoopStreamLine
		if unmarshal.Unmarshal(line, &l) != nil {
			// 读取出错（如超过上限）时 Scanner 仍交出不完整的最后一行：报读取的错误，而不是"无法解析"。
			if err := sc.Err(); err != nil {
				return nil, err
			}
			return nil, errUnparsable
		}
		switch x := l.GetLine().(type) {
		case *v1.LoopStreamLine_Delta:
			if onDelta != nil && (x.Delta.GetText() != "" || x.Delta.GetReasoning() != "") {
				onDelta(model.Delta{Text: x.Delta.GetText(), Reasoning: x.Delta.GetReasoning()})
			}
		case *v1.LoopStreamLine_Response:
			out = x.Response
		default:
			return nil, errors.New("empty stream line")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("stream ended without the response line")
	}
	return out, nil
}

// Request 把一步转成线上的请求。
func Request(step runtime.LoopStep) *v1.LoopStepRequest {
	out := &v1.LoopStepRequest{
		SessionId: step.SessionID, RunId: step.RunID, Attempt: step.Attempt, Turn: step.Turn, EndUser: step.EndUser,
		Agent: step.Agent, State: step.State, StateVersion: step.StateVersion,
	}
	if r := step.Request; r != nil {
		out.System = r.System
		for _, m := range r.Messages {
			out.Messages = append(out.Messages, &v1.LoopMessage{
				Role: string(m.Role), Content: Blocks(m.Content, m.Role == model.RoleUser), ToolCalls: m.ToolCalls, ToolCallId: m.ToolCallID,
			})
		}
		for _, t := range r.Tools {
			out.Tools = append(out.Tools, &v1.LoopTool{Name: t.Name, Description: t.Description, InputSchemaJson: string(t.InputSchema)})
		}
	}
	if len(step.Previous) > 0 {
		out.Previous = &v1.LoopPreviousStep{}
		for i, p := range step.Previous {
			out.Previous.Results = append(out.Previous.Results, &v1.LoopCallResult{
				Index: uint32(i), CallId: p.Call.GetCallId(), Capability: p.Call.GetCapability(),
				Content: Blocks(p.Content, false), IsError: p.IsError,
			})
		}
	}
	return out
}

// Blocks 把内容呈现为外部服务看到的形式：与模型提供商相同（internal/model/openaicompat 的 content），只有文本与媒体；
// 卡片不发送（结果中另有说明），图片与音频只在用户消息中且已内联时发送，其余媒体呈现为一行描述。
func Blocks(blocks []*v1.ContentBlock, allowMedia bool) []*v1.ContentBlock {
	var out []*v1.ContentBlock
	for _, b := range blocks {
		switch k := b.GetKind().(type) {
		case *v1.ContentBlock_Text:
			out = append(out, b)
		case *v1.ContentBlock_UiContext:
			out = append(out, model.TextBlocks(model.UIContextText(k.UiContext))...)
		case *v1.ContentBlock_CardAction:
			out = append(out, model.TextBlocks(model.CardActionText(k.CardAction))...)
		case *v1.ContentBlock_Media:
			mt := k.Media.GetMimeType()
			if allowMedia && len(k.Media.GetData()) > 0 && (strings.HasPrefix(mt, "image/") || strings.HasPrefix(mt, "audio/")) {
				out = append(out, b)
			} else {
				out = append(out, model.TextBlocks(model.DescribeMedia(k.Media))...)
			}
		}
	}
	return out
}

// Reply 把线上的回答转成 runtime.LoopReply。校验（工具、内容、状态上限）在 Worker 中进行。
func Reply(r *v1.LoopStepResponse) *runtime.LoopReply {
	out := &runtime.LoopReply{
		Response:     &model.Response{Content: r.GetContent(), ToolCalls: r.GetToolCalls(), Reasoning: r.GetReasoning()},
		State:        r.GetState(),
		StateVersion: r.GetStateVersion(),
	}
	for _, u := range r.GetReportedUsage() {
		out.Reported = append(out.Reported, runtime.ReportedUsage{Model: u.GetModel(), Usage: u.GetUsage()})
	}
	return out
}
