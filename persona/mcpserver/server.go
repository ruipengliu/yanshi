// Package mcpserver 把 Persona 的领域操作装配成 streamable HTTP MCP。
// 调用身份与引用来自可信调用方的 _meta（docs/design/m5-mcp.md、captures.md §6）；
// 此 Handler 本身不鉴权，生产部署须由应用入口认证平台，不能向不可信客户端直接开放。
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"persona/domain"
	"persona/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registration 是 xinli 的登记内容。应用层据此生成内核登记，避免多处维护工具白名单。
type Registration struct {
	Name            string        `json:"name"`
	NoApproval      []string      `json:"no_approval"`
	CiteCaptures    bool          `json:"cite_captures"`
	AttestsSpeakers bool          `json:"attests_speakers"`
	Acks            bool          `json:"acks"`
	Timeout         time.Duration `json:"timeout"`
}

func XinliRegistration() Registration {
	return Registration{Name: "xinli", NoApproval: []string{"file_changes", "file_day", "set_status", "update_commitments", "propose_merge", "file_week"},
		CiteCaptures: true, Acks: false, AttestsSpeakers: true, Timeout: 30 * time.Second}
}

// Notifier 是接受事实后的进程内提示；可靠发现来自同事务保存的领域变化（Q99）。
// 只在事务提交成功、锁释放后同步通知；重复交出会重复通知，编排器须按 Session 与项目去重。
// TODO(P4-A8/A10)：应用持久接收领域变化后再确认；此提示不能代替恢复扫描。
type Notifier interface {
	DayFiled(ctx context.Context, businessLine, endUser, daySession, date string, projects []string)
}

// DeletionNotifier 与 DayFiled 分开，现有编排器可以在④b 接入删除待办。
// 只在事务提交、锁释放后通知待办 ID；执行与重试不属于 Persona。
type DeletionNotifier interface {
	DeletionPending(ctx context.Context, businessLine, endUser, taskID string)
}

type Options struct {
	// AllowUnmanaged 只供旧夹具与评测；生产必须登记工作命令。
	AllowUnmanaged bool
	Now            func() time.Time
	Notifier       Notifier
}
type server struct {
	allowUnmanaged bool
	store          store.Store
	now            func() time.Time
	notifier       Notifier
}

func newRuntime(s store.Store, opts Options) *server {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &server{allowUnmanaged: opts.AllowUnmanaged, store: s, now: now, notifier: opts.Notifier}
}

// Handler 不监听端口或拥有进程生命周期，调用方可与夹具或业务 API 装配在同一 HTTP 服务中。
func Handler(s store.Store, opts Options) http.Handler {
	x := newRuntime(s, opts)
	srv := mcp.NewServer(&mcp.Implementation{Name: XinliRegistration().Name, Version: "3"}, nil)
	srv.AddTool(capturesTool, x.captures)
	for _, t := range tools {
		srv.AddTool(t.tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return x.call(ctx, req.Params.Meta, req.Params.Arguments, t.handler, t.tool.Annotations != nil && t.tool.Annotations.ReadOnlyHint)
		})
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

func (x *server) call(ctx context.Context, metadata mcp.Meta, args json.RawMessage, handler func(*domain.Call) string, readOnly bool) (*mcp.CallToolResult, error) {
	// 线上字段名见 docs/design/captures.md §6，不依赖内核 Go 类型。
	meta := func(k string) string {
		value, _ := metadata["yanshi/"+k].(string)
		return value
	}
	bl, eu, session := meta("business_line"), meta("end_user"), meta("session")
	cites := citations(metadata["yanshi/captures"])
	var call *domain.Call
	var text string
	now := x.now()
	apply := func(book *domain.Book) error {
		call = domain.NewCall(book, session, args, cites, now)
		book.SetCaptureClock(x.now)
		call.RequireWork = !x.allowUnmanaged
		if err := call.CheckWorkTool(false); err != nil {
			text = err.Error()
			return nil
		}
		text = handler(call)
		return call.Err()
	}
	transact := x.store.Update
	if readOnly {
		transact = x.store.View
	}
	if err := transact(ctx, bl, eu, apply); err != nil {
		return nil, err
	}
	if call.Filed != nil && x.notifier != nil {
		day := call.Filed
		x.notifier.DayFiled(ctx, bl, eu, day.Session, day.Date, day.Projects)
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	return result, nil
}

func citations(value any) map[string]domain.Citation {
	raw, _ := json.Marshal(value)
	var list []domain.Citation
	_ = json.Unmarshal(raw, &list)
	out := make(map[string]domain.Citation, len(list))
	for _, cite := range list {
		if cite.TimeZone != "" {
			if location, err := time.LoadLocation(cite.TimeZone); err == nil {
				cite.SpokenAt = cite.SpokenAt.In(location)
			}
		}
		out[cite.Ref] = cite
	}
	return out
}
