// Package yanshi 是 Go SDK（sdk/nodesdk）面向移动端的绑定层：gomobile bind 生成 iOS 的 xcframework
// 与 Android 的 aar（make mobile）。
//
// gomobile 只能跨语言传递基本类型、[]byte、结构体指针与接口，所以：
//   - 契约中的消息（Event、LiveDelta、Presence、ClientRequest / ClientResponse、CapabilitySpec、Invoke /
//     InvokeResult、ContentBlock）一律以 protobuf 字节传递，原生侧用 swift-protobuf / protobuf-javalite
//     从 proto/ 生成的类型编解码；
//   - 回调是原生侧实现的接口，在 SDK 的 goroutine 中调用（不是主线程），更新界面前切回主线程；
//   - 阻塞的方法（Request、UploadArtifact）应在后台线程调用。
//
// 语义与 nodesdk 相同：断线重连并按 seq 续订，带输入 ID 的提交在断线后自动重试，调用以 call_id 去重执行
// （docs/design/m1-device-nodes.md、m3-duplex-channel.md）。
package yanshi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
)

// Config 是连接配置。
type Config struct {
	// URL 是网关地址，如 wss://example.com/v1/connect。
	URL string
	// NodeID 是设备 ID：须持久保存，跨重启不变。
	NodeID string
	// Token 是固定令牌，仅在未设置 TokenSource 时使用（开发）。
	Token        string
	BusinessLine string
	EndUser      string
	Label        string
	// Kind 是设备类型，如 "phone"。
	Kind    string
	HostApp string
	// PushPlatform 是推送通道（如 "apns"、"fcm"）；令牌由 PushTokenSource 提供。
	PushPlatform string
	// LedgerDir 是调用账本的目录（应用的私有目录）。声明了 Capability 时必填：
	// 账本保证 App 被杀后重启时，有副作用的调用不会重复执行。
	LedgerDir string
}

// TokenSource 返回业务线签发的用户令牌，每次连接时调用（在后台线程，可以阻塞去刷新）。
// 暂时取不到时返回空串：这次连接放弃，按退避稍后重试。（不用 error 返回值：gomobile 对返回字符串的方法
// 不生成 Swift 的 throws，实现起来很别扭。）
type TokenSource interface {
	Token() string
}

// ErrNoToken 表示 TokenSource 返回了空串。
var ErrNoToken = errors.New("yanshi: token source returned no token")

func tokenFrom(src TokenSource) (string, error) {
	if t := src.Token(); t != "" {
		return t, nil
	}
	return "", ErrNoToken
}

// PushTokenSource 返回推送通道当前的设备令牌，每次连接时调用；返回空串时不登记推送。
type PushTokenSource interface {
	PushToken() string
}

// ConnectionObserver 在每次连接成功后收到网关分配的标签。
type ConnectionObserver interface {
	OnConnected(label string)
}

// CapabilityHandler 执行一次调用。invoke 是序列化的 yanshi.v1.Invoke（含 session_id、call_id、arguments_json）；
// 返回序列化的 yanshi.v1.InvokeResult，SDK 只取其中的 content 与 is_error。返回 error 时，错误信息作为错误结果交给 Agent。
//
// 同一 call_id 只会执行一次（SDK 以账本去重）。调用被取消时 SDK 不再等待结果，但无法中止原生代码。
type CapabilityHandler interface {
	Execute(invoke []byte) ([]byte, error)
}

// SessionHandler 接收一个订阅的消息，参数为序列化的 yanshi.v1.Event / LiveDelta / Presence。
// 回调在连接的接收 goroutine 中按到达顺序串行调用，不应阻塞。
type SessionHandler interface {
	OnEvent(event []byte)
	// OnDelta 收到实时增量：snapshot 时替换草稿，end 时丢弃未提交的草稿（docs/design/m3-duplex-channel.md §4）。
	OnDelta(delta []byte)
	// OnPresence 收到在场设备列表（订阅后先收到一次，之后每次变化时收到全量）。
	OnPresence(presence []byte)
	// OnEnded 在网关结束订阅时调用："not_found"、"deleted"、"limit" 或 "error"（可重新订阅）。
	OnEnded(reason string)
}

// Client 是一条到网关的连接，兼任会话客户端与（声明了 Capability 时）Node。
type Client struct {
	cfg Config

	mu       sync.Mutex
	tokens   TokenSource
	push     PushTokenSource
	observer ConnectionObserver
	caps     []nodesdk.Capability
	c        *nodesdk.Client
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewClient 创建客户端；设置回调与 Capability 后调用 Start。
func NewClient(cfg *Config) *Client {
	return &Client{cfg: *cfg}
}

func (c *Client) SetTokenSource(source TokenSource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = source
}

func (c *Client) SetPushTokenSource(source PushTokenSource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.push = source
}

func (c *Client) SetObserver(observer ConnectionObserver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observer = observer
}

// ErrStarted 表示客户端已启动：Capability 须在 Start 之前声明。
var ErrStarted = errors.New("yanshi: client already started")

// ErrNotStarted 表示客户端尚未启动或已停止。
var ErrNotStarted = errors.New("yanshi: client not started")

// AddCapability 声明一项本机能力；spec 是序列化的 yanshi.v1.CapabilitySpec。须在 Start 之前调用。
func (c *Client) AddCapability(spec []byte, handler CapabilityHandler) error {
	s := &v1.CapabilitySpec{}
	if err := proto.Unmarshal(spec, s); err != nil {
		return fmt.Errorf("yanshi: capability spec: %w", err)
	}
	if s.GetName() == "" {
		return errors.New("yanshi: capability spec: name is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c != nil {
		return ErrStarted
	}
	c.caps = append(c.caps, nodesdk.Capability{Spec: s, Handler: bridge(handler)})
	return nil
}

func bridge(h CapabilityHandler) nodesdk.Handler {
	return func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
		inv, err := proto.Marshal(nodesdk.Invocation(ctx))
		if err != nil {
			return nil, err
		}
		// 原生代码不可中止：在单独的 goroutine 中执行，取消时不再等待。
		type outcome struct {
			b   []byte
			err error
		}
		ch := make(chan outcome, 1)
		go func() {
			b, err := h.Execute(inv)
			ch <- outcome{b, err}
		}()
		var o outcome
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case o = <-ch:
		}
		if o.err != nil {
			return nil, o.err
		}
		res := &v1.InvokeResult{}
		if err := proto.Unmarshal(o.b, res); err != nil {
			return nil, fmt.Errorf("capability returned an invalid InvokeResult: %w", err)
		}
		if res.GetIsError() {
			return nil, errors.New(textOf(res.GetContent()))
		}
		return res.GetContent(), nil
	}
}

func textOf(blocks []*v1.ContentBlock) string {
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.GetText().GetText())
	}
	return b.String()
}

// Start 开始连接；断开后以指数退避重连，直到 Stop。
func (c *Client) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c != nil {
		return ErrStarted
	}
	cfg := nodesdk.Config{URL: c.cfg.URL, NodeID: c.cfg.NodeID, Token: c.cfg.Token, BusinessLine: c.cfg.BusinessLine,
		EndUser: c.cfg.EndUser, Label: c.cfg.Label, Kind: c.cfg.Kind, HostApp: c.cfg.HostApp, PushPlatform: c.cfg.PushPlatform}
	if src := c.tokens; src != nil {
		cfg.TokenSource = func(context.Context) (string, error) { return tokenFrom(src) }
	}
	if src := c.push; src != nil {
		cfg.PushToken = src.PushToken
	}
	if o := c.observer; o != nil {
		cfg.OnConnected = o.OnConnected
	}
	if len(c.caps) > 0 {
		if c.cfg.LedgerDir == "" {
			return errors.New("yanshi: LedgerDir is required when capabilities are declared")
		}
		cfg.Executor = nodesdk.NewExecutor(nodesdk.FileLedger{Dir: c.cfg.LedgerDir}, c.caps...)
	}
	c.c = nodesdk.NewClient(cfg)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.done = make(chan struct{})
	go func(nc *nodesdk.Client, ctx context.Context, done chan struct{}) {
		defer close(done)
		_ = nc.Run(ctx)
	}(c.c, c.ctx, c.done)
	return nil
}

// Stop 关闭连接并等待后台 goroutine 结束；未完成的请求以错误结束。之后不能再 Start。
func (c *Client) Stop() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (c *Client) client() (*nodesdk.Client, context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return nil, nil, ErrNotStarted
	}
	return c.c, c.ctx, nil
}

// Subscription 是一个订阅；Cancel 取消它。
type Subscription struct{ cancel func() }

func (s *Subscription) Cancel() { s.cancel() }

// Subscribe 订阅 Session 中 seq 大于 afterSeq 的事件与之后的实时增量；未连接时在连接建立后自动订阅，
// 重连后按最后收到的 seq 续订。同一 Session 重复订阅时以新的为准。
func (c *Client) Subscribe(sessionID string, afterSeq int64, handler SessionHandler) (*Subscription, error) {
	nc, _, err := c.client()
	if err != nil {
		return nil, err
	}
	if afterSeq < 0 {
		afterSeq = 0
	}
	return &Subscription{cancel: nc.Subscribe(sessionID, uint64(afterSeq), sessionHandler{handler})}, nil
}

type sessionHandler struct{ h SessionHandler }

func marshal(m proto.Message) []byte {
	b, _ := proto.Marshal(m)
	return b
}

func (s sessionHandler) OnEvent(e *v1.Event)       { s.h.OnEvent(marshal(e)) }
func (s sessionHandler) OnDelta(d *v1.LiveDelta)   { s.h.OnDelta(marshal(d)) }
func (s sessionHandler) OnPresence(p *v1.Presence) { s.h.OnPresence(marshal(p)) }
func (s sessionHandler) OnEnded(reason string)     { s.h.OnEnded(reason) }

// 实现 PresenceHandler，网关的在场列表才会转给原生侧。
var _ nodesdk.PresenceHandler = sessionHandler{}

// SetActivity 报告本设备在一个已订阅 Session 中的状态：focused 表示该 Session 正显示在前台
// （有设备在看时不推送提醒），typing 表示正在输入（网关 10 秒后视为停止）。
func (c *Client) SetActivity(sessionID string, focused, typing bool) {
	if nc, _, err := c.client(); err == nil {
		nc.SetActivity(sessionID, focused, typing)
	}
}

// Request 发出一个请求（序列化的 yanshi.v1.ClientRequest）并返回序列化的 yanshi.v1.ClientResponse；
// request_id 由 SDK 填写。未连接时等待连接建立。
//
// 网关拒绝请求时，响应的 error 字段给出原因（code 与 HTTP 状态码对应），不返回 error；
// 返回 error 表示没有得到响应：超时（timeoutMillis > 0）、客户端停止，或连接断开导致结果未知。
// 带 input_id 的提交在连接断开后自动以同一 ID 重试（按 ID 去重，恰好生效一次）。
func (c *Client) Request(request []byte, timeoutMillis int64) ([]byte, error) {
	nc, ctx, err := c.client()
	if err != nil {
		return nil, err
	}
	req := &v1.ClientRequest{}
	if err := proto.Unmarshal(request, req); err != nil {
		return nil, fmt.Errorf("yanshi: request: %w", err)
	}
	if timeoutMillis > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMillis)*time.Millisecond)
		defer cancel()
	}
	var resp *v1.ClientResponse
	if in := req.GetSubmit(); in != nil {
		resp, err = nc.SubmitInput(ctx, in)
	} else {
		resp, err = nc.Request(ctx, req)
	}
	var re *nodesdk.RequestError
	if errors.As(err, &re) {
		resp, err = &v1.ClientResponse{RequestId: req.GetRequestId(), Error: &v1.ClientError{Code: re.Code, Message: re.Message}}, nil
		if !re.RetryAt.IsZero() {
			resp.Error.RetryAt = timestamppb.New(re.RetryAt)
		}
	}
	if err != nil {
		return nil, err
	}
	return proto.Marshal(resp)
}

// SubmitText 提交一段文本（新的输入 ID，断线自动重试），返回序列化的 ClientResponse，含义同 Request。
func (c *Client) SubmitText(sessionID, text string, timeoutMillis int64) ([]byte, error) {
	req, err := proto.Marshal(&v1.ClientRequest{Op: &v1.ClientRequest_Submit{Submit: &v1.SubmitInput{
		SessionId: sessionID, InputId: NewInputID(),
		Input: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: text}}}}}}})
	if err != nil {
		return nil, err
	}
	return c.Request(req, timeoutMillis)
}

// NewInputID 生成一个随机的输入 ID。需要跨 App 重启继续重试的提交，持久保存它并放进 SubmitInput.input_id。
func NewInputID() string { return nodesdk.NewInputID() }

// UploadArtifact 把文件上传为 sessionID 的工件，返回序列化的 yanshi.v1.ContentBlock（Media，引用 artifact://），
// 可放入调用结果或输入。通常在 CapabilityHandler 中调用，sessionID 取自 Invoke。
func (c *Client) UploadArtifact(sessionID, name, mimeType string, data []byte) ([]byte, error) {
	_, ctx, err := c.client()
	if err != nil {
		return nil, err
	}
	base, err := nodesdk.APIBaseFromGateway(c.cfg.URL)
	if err != nil {
		return nil, err
	}
	arts := &nodesdk.Artifacts{BaseURL: base}
	c.mu.Lock()
	src, fixed := c.tokens, c.cfg.Token
	c.mu.Unlock()
	arts.TokenSource = func(context.Context) (string, error) {
		if src != nil {
			return tokenFrom(src)
		}
		return fixed, nil
	}
	b, err := arts.Upload(ctx, sessionID, name, mimeType, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return proto.Marshal(b)
}
