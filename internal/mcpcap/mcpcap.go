// Package mcpcap 把业务线登记的远程 MCP Server（Streamable HTTP）接成 Capability
// （docs/design/m5-mcp.md §2–§4，ADR-0017）。调用由 Worker 进程内的受信客户端以业务线级凭证发起，
// 语义与进程内 Capability 相同：执行前落盘 ToolCallStarted，崩溃后按幂等性重试或返回"结果未知"。
package mcpcap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/sdk/nodesdk"
)

// Server 是一个远程 MCP Server 的登记（mcp/*.yaml）。
type Server struct {
	Name         string            `yaml:"name"`
	BusinessLine string            `yaml:"business_line"`
	URL          string            `yaml:"url"`
	Headers      map[string]string `yaml:"headers"`
	Timeout      time.Duration     `yaml:"timeout"`
	Tools        struct {
		// Allow 为空表示开放全部工具。
		Allow []string `yaml:"allow"`
		// NoApproval 是业务线确认无需审批的工具；明确声明 destructiveHint 的工具不受影响。
		NoApproval []string `yaml:"no_approval"`
	} `yaml:"tools"`
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)
	varRE  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// LoadDir 读取 dir 下所有 *.yaml 登记；目录不存在时返回空。请求头中的 ${VAR} 从环境变量注入，
// 使凭证不以明文出现在登记文件中。
func LoadDir(dir string) ([]Server, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Server
	names := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var s Server
		if err := yaml.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := s.validate(os.Getenv); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if names[s.Name] {
			return nil, fmt.Errorf("%s: MCP server %q registered twice", f, s.Name)
		}
		names[s.Name] = true
		out = append(out, s)
	}
	return out, nil
}

func (s *Server) validate(getenv func(string) string) error {
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("name %q must match %s", s.Name, nameRE)
	}
	if s.BusinessLine == "" || !(strings.HasPrefix(s.URL, "https://") || strings.HasPrefix(s.URL, "http://")) {
		return fmt.Errorf("business_line and an http(s) url are required")
	}
	if s.Timeout <= 0 {
		s.Timeout = 30 * time.Second
	}
	for k, v := range s.Headers {
		var missing []string
		s.Headers[k] = varRE.ReplaceAllStringFunc(v, func(m string) string {
			name := varRE.FindStringSubmatch(m)[1]
			val := getenv(name)
			if val == "" {
				missing = append(missing, name)
			}
			return val
		})
		if len(missing) > 0 {
			return fmt.Errorf("header %s: environment variables %v are not set", k, missing)
		}
	}
	for _, g := range append(append([]string{}, s.Tools.Allow...), s.Tools.NoApproval...) {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("bad tool pattern %q: %w", g, err)
		}
	}
	return nil
}

func matchAny(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

// ToolName 是模型看到的工具名 "mcp_<server>__<tool>"。设备标签不含下划线，不会与 "<label>__<capability>" 冲突。
func ToolName(server, tool string) string {
	return "mcp_" + server + "__" + nodesdk.SafeName(tool, 64-len("mcp_")-len(server)-len("__"))
}

// Connector 管理到各远程 MCP Server 的连接与工具缓存，实现 capability.MCPSource。
type Connector struct {
	Servers []Server
	// Artifacts 非 nil 时，结果中的图片、音频保存为 Session 的工件。
	Artifacts *artifact.Service
	// HTTPClient 为 nil 时使用默认客户端。
	HTTPClient *http.Client
	Logger     *slog.Logger
	// CacheTTL 是工具列表的缓存时长，默认 5 分钟。
	CacheTTL time.Duration

	mu    sync.Mutex
	conns map[string]*conn
}

type conn struct {
	session *mcp.ClientSession
	tools   []*mcp.Tool
	listed  time.Time
}

var _ capability.MCPSource = (*Connector)(nil)

func (c *Connector) log() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

type identityKey struct{}

type identity struct{ businessLine, endUser, session string }

// transport 注入登记中的凭证，并以请求头传递调用方身份（docs/design/m5-mcp.md §3）。
type transport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	if id, ok := r.Context().Value(identityKey{}).(identity); ok {
		r.Header.Set("X-Yanshi-Business-Line", id.businessLine)
		r.Header.Set("X-Yanshi-End-User", id.endUser)
		r.Header.Set("X-Yanshi-Session", id.session)
	}
	return t.base.RoundTrip(r)
}

func (c *Connector) connect(ctx context.Context, s Server) (*mcp.ClientSession, error) {
	base := http.DefaultTransport
	if c.HTTPClient != nil && c.HTTPClient.Transport != nil {
		base = c.HTTPClient.Transport
	}
	hc := &http.Client{Transport: transport{base: base, headers: s.Headers}}
	client := mcp.NewClient(&mcp.Implementation{Name: "yanshi", Version: "1"}, nil)
	cctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	// 只用请求-响应：不建立常驻的 SSE 流，工具变化靠缓存过期与调用失败时刷新发现。
	return client.Connect(cctx, &mcp.StreamableClientTransport{Endpoint: s.URL, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
}

// state 返回 Server 的连接与工具列表；refresh 为 true 时强制重新列出工具。
func (c *Connector) state(ctx context.Context, s Server, refresh bool) (*conn, error) {
	ttl := c.CacheTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c.mu.Lock()
	if c.conns == nil {
		c.conns = map[string]*conn{}
	}
	cur := c.conns[s.Name]
	c.mu.Unlock()
	if cur != nil && !refresh && time.Since(cur.listed) < ttl {
		return cur, nil
	}
	session := (*mcp.ClientSession)(nil)
	if cur != nil {
		session = cur.session
	}
	var tools []*mcp.Tool
	list := func() error {
		tools = tools[:0]
		lctx, cancel := context.WithTimeout(ctx, s.Timeout)
		defer cancel()
		for t, err := range session.Tools(lctx, nil) {
			if err != nil {
				return err
			}
			tools = append(tools, t)
		}
		return nil
	}
	if session == nil || list() != nil {
		// 首次连接，或连接已失效：重连一次。
		if session != nil {
			_ = session.Close()
		}
		var err error
		if session, err = c.connect(ctx, s); err != nil {
			return nil, err
		}
		if err := list(); err != nil {
			_ = session.Close()
			return nil, err
		}
	}
	next := &conn{session: session, tools: tools, listed: time.Now()}
	c.mu.Lock()
	c.conns[s.Name] = next
	c.mu.Unlock()
	return next, nil
}

// Tools 返回业务线 businessLine 可用、且匹配 patterns（"<server>/<glob>"，已去掉 "mcp:" 前缀）的工具。
// 某个 Server 不可用时跳过它（记录日志），不影响其他工具。
func (c *Connector) Tools(ctx context.Context, businessLine string, patterns []string) ([]capability.Tool, error) {
	var out []capability.Tool
	for _, s := range c.Servers {
		if s.BusinessLine != businessLine {
			continue
		}
		var globs []string
		for _, p := range patterns {
			srv, glob, ok := strings.Cut(p, "/")
			if !ok {
				return nil, fmt.Errorf("bad MCP capability pattern %q: want mcp:<server>/<glob>", capability.MCPPrefix+p)
			}
			if m, _ := path.Match(srv, s.Name); m {
				globs = append(globs, glob)
			}
		}
		if len(globs) == 0 {
			continue
		}
		st, err := c.state(ctx, s, false)
		if err != nil {
			c.log().Warn("MCP server unavailable", "server", s.Name, "err", err)
			continue
		}
		for _, t := range st.tools {
			if !matchAny(globs, t.Name) || (len(s.Tools.Allow) > 0 && !matchAny(s.Tools.Allow, t.Name)) {
				continue
			}
			out = append(out, c.tool(s, t))
		}
	}
	return out, nil
}

// tool 把 MCP 工具映射为 Capability：只读免审批；业务线可用 no_approval 放宽，但明确声明 destructiveHint 的不行。
func (c *Connector) tool(s Server, t *mcp.Tool) capability.Tool {
	risk, idem := nodesdk.MCPRisk(t)
	if risk == v1.Risk_RISK_HIGH && matchAny(s.Tools.NoApproval, t.Name) && !nodesdk.MCPDestructive(t) {
		risk = v1.Risk_RISK_LOW
	}
	schema, err := json.Marshal(t.InputSchema)
	if err != nil || string(schema) == "null" {
		schema = []byte(`{"type":"object"}`)
	}
	spec := capability.Spec{Name: ToolName(s.Name, t.Name), Description: t.Description, InputSchema: schema, Idempotent: idem, Risk: risk}
	name := t.Name
	return capability.Tool{Spec: spec, Local: capability.Func{S: spec, Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
		return c.call(ctx, s, name, inv)
	}}}
}

func (c *Connector) call(ctx context.Context, s Server, tool string, inv capability.Invocation) ([]*v1.ContentBlock, error) {
	st, err := c.state(ctx, s, false)
	if err != nil {
		return nil, fmt.Errorf("MCP server %s unavailable: %w", s.Name, err)
	}
	ctx = context.WithValue(ctx, identityKey{}, identity{inv.BusinessLine, inv.EndUser, inv.SessionID})
	cctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	args := json.RawMessage(inv.Arguments)
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := st.session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: mcp.Meta{
		"yanshi/business_line": inv.BusinessLine, "yanshi/end_user": inv.EndUser, "yanshi/session": inv.SessionID,
	}})
	if err != nil {
		if ctx.Err() == nil && cctx.Err() == nil {
			// 连接或协议错误：下次调用前重新连接并刷新工具列表。
			c.mu.Lock()
			delete(c.conns, s.Name)
			c.mu.Unlock()
		}
		return nil, fmt.Errorf("MCP %s/%s: %w", s.Name, tool, err)
	}
	var upload nodesdk.Uploader
	if c.Artifacts != nil {
		upload = func(ctx context.Context, name, mimeType string, data []byte) (*v1.ContentBlock, error) {
			m, err := c.Artifacts.Put(ctx, inv.SessionID, name, mimeType, bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			return &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{Uri: artifact.URI(m.ID), Name: m.Name, MimeType: m.MimeType, Size: uint64(m.Size)}}}, nil
		}
	}
	content, isErr, err := nodesdk.MCPContent(ctx, res, upload)
	if err != nil {
		return nil, err
	}
	if isErr {
		var b strings.Builder
		for _, x := range content {
			b.WriteString(x.GetText().GetText())
		}
		return nil, fmt.Errorf("%s", b.String())
	}
	return content, nil
}

// Close 关闭全部连接。
func (c *Connector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.conns {
		_ = x.session.Close()
	}
	c.conns = nil
}
