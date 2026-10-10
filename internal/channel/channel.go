// Package channel 是 Connection 的会话客户端角色（docs/design/m3-duplex-channel.md，ADR-0024）：
// 在同一条连接上订阅多个 Session 的事件流，并执行客户端请求（创建 Session、输入、中断、审批、关闭）。
// 它与传输无关：wsgateway 负责收发帧，测试可以直接驱动 Conn。
package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/clock"
	"yanshi/internal/feed"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/moderation"
	"yanshi/internal/presence"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
)

// MaxSubscriptions 限制一条连接同时订阅的 Session 数。
const MaxSubscriptions = 32

// maxInflight 限制一条连接同时执行的请求数：请求在独立的 goroutine 中执行，
// 以免内容安全检查等慢操作阻塞同一连接上的设备结果。
const maxInflight = 8

// Identity 是连接认证得到的 EndUser 与设备；会话客户端只能访问该 EndUser 在该业务线下的 Session。
type Identity struct {
	BusinessLine string
	EndUser      string
	// 设备信息来自 Hello，用于在场显示。
	DeviceID string
	Label    string
	Kind     string
}

type Handler struct {
	Service *service.Service
	Live    live.Bus
	// Presence 为 nil 时不记录、不推送在场（§6）。
	Presence *presence.Service
	Clock    clock.Clock
	// IDs 生成连接 ID，默认随机。
	IDs    ids.Generator
	Logger *slog.Logger
}

func (h *Handler) log() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// Conn 是一条连接上的会话客户端状态。send 可被多个 goroutine 并发调用，须自行串行化。
type Conn struct {
	h      *Handler
	id     Identity
	connID string
	send   func(*v1.GatewayMessage) error

	ctx      context.Context
	cancel   context.CancelFunc
	inflight chan struct{}
	wg       sync.WaitGroup

	mu   sync.Mutex
	subs map[string]*subscription
	// last 是每个 Session 最近一次的订阅（含已退订、尚未退出的）：新订阅须等它退出后再写在场记录。
	last map[string]*subscription
}

type subscription struct {
	cancel context.CancelFunc
	// done 在订阅的 goroutine 退出（含撤下在场记录）后关闭。
	done chan struct{}
	// 以下由 Conn.mu 保护。joined 表示已通过授权并写入在场记录，此后 Activity 才写入存储。
	joined      bool
	focused     bool
	typingUntil time.Time
}

func (h *Handler) now() time.Time {
	if h.Clock == nil {
		return time.Now()
	}
	return h.Clock.Now()
}

// Open 为一条已认证的连接创建会话客户端状态；连接结束时调用 Close。
func (h *Handler) Open(ctx context.Context, id Identity, send func(*v1.GatewayMessage) error) *Conn {
	ctx, cancel := context.WithCancel(ctx)
	gen := h.IDs
	if gen == nil {
		gen = ids.Random()
	}
	c := &Conn{h: h, id: id, connID: "conn_" + gen(), send: send, ctx: ctx, cancel: cancel,
		inflight: make(chan struct{}, maxInflight), subs: map[string]*subscription{}, last: map[string]*subscription{}}
	if h.Presence != nil {
		c.wg.Add(1)
		go func() { defer c.wg.Done(); c.heartbeat() }()
	}
	return c
}

// heartbeat 定期为本连接的全部在场记录续期。
func (c *Conn) heartbeat() {
	t := time.NewTicker(presence.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			if err := c.h.Presence.Store.Touch(c.ctx, c.connID, c.h.now().Add(presence.TTL)); err != nil && c.ctx.Err() == nil {
				c.h.log().Warn("presence heartbeat failed", "err", err)
			}
		}
	}
}

// Close 结束全部订阅与进行中的请求，并等待它们退出。
func (c *Conn) Close() {
	c.cancel()
	c.wg.Wait()
}

// Handle 处理一条会话客户端消息；不属于会话客户端的消息返回 false。
func (c *Conn) Handle(m *v1.NodeMessage) bool {
	switch m := m.GetMsg().(type) {
	case *v1.NodeMessage_Subscribe:
		c.subscribe(m.Subscribe)
	case *v1.NodeMessage_Unsubscribe:
		c.unsubscribe(m.Unsubscribe.GetSessionId())
	case *v1.NodeMessage_Activity:
		c.activity(m.Activity)
	case *v1.NodeMessage_Request:
		// 占满时在读循环中等待：对过快的客户端施加背压，而不是无限开 goroutine。
		select {
		case c.inflight <- struct{}{}:
		case <-c.ctx.Done():
			return true
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer func() { <-c.inflight }()
			_ = c.send(&v1.GatewayMessage{Msg: &v1.GatewayMessage_Response{Response: c.h.Request(c.ctx, c.id, m.Request)}})
		}()
	default:
		return false
	}
	return true
}

func (c *Conn) subscribe(sub *v1.Subscribe) {
	sid := sub.GetSessionId()
	c.mu.Lock()
	old := c.subs[sid]
	if old != nil {
		// 重复订阅以新的 after_seq 重新开始。
		old.cancel()
	} else if len(c.subs) >= MaxSubscriptions {
		c.mu.Unlock()
		c.ended(sid, "limit")
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	s := &subscription{cancel: cancel, done: make(chan struct{})}
	if old != nil {
		// 沿用旧订阅的设备状态。
		s.focused, s.typingUntil = old.focused, old.typingUntil
	}
	prev := c.last[sid]
	c.subs[sid], c.last[sid] = s, s
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(s.done)
		// 等上一个订阅（被取代或已退订）退出：它撤下在场记录用的是同一个 (Session, 连接) 键，不能晚于新订阅写入。
		if prev != nil {
			<-prev.done
		}
		reason := c.stream(ctx, s, sid, sub.GetAfterSeq())
		c.mu.Lock()
		mine := c.subs[sid] == s
		if mine {
			delete(c.subs, sid)
		}
		if c.last[sid] == s {
			delete(c.last, sid)
		}
		c.mu.Unlock()
		cancel()
		// 被取消（退订、被新订阅取代、连接关闭）时不通知。
		if reason != "" && mine && c.ctx.Err() == nil {
			c.ended(sid, reason)
		}
	}()
}

func (c *Conn) unsubscribe(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.subs[sid]; s != nil {
		s.cancel()
		delete(c.subs, sid)
	}
}

func (c *Conn) ended(sid, reason string) {
	_ = c.send(&v1.GatewayMessage{Msg: &v1.GatewayMessage_SubscriptionEnded{SubscriptionEnded: &v1.SubscriptionEnded{
		SessionId: sid, Reason: reason}}})
}

// stream 推送一个订阅直到结束，返回结束原因；被取消时返回空串。
func (c *Conn) stream(ctx context.Context, s *subscription, sid string, after uint64) string {
	h := c.h
	st, err := h.load(ctx, c.id, sid)
	if errors.Is(err, service.ErrNotFound) {
		return "not_found"
	}
	if err == nil && h.Presence != nil {
		// 订阅结束（含连接关闭）时撤下在场记录；ctx 此时可能已取消。
		defer func() {
			if err := h.Presence.Store.Remove(context.WithoutCancel(ctx), sid, c.connID); err != nil {
				h.log().Warn("presence remove failed", "session", sid, "err", err)
			}
		}()
		err = c.join(ctx, s, sid)
	}
	if err == nil {
		src := feed.Source{Log: h.Service.Store.Log, Live: h.Live, Deletions: h.Service.Deletions}
		if h.Presence == nil {
			err = src.Stream(ctx, st, after, sink{send: c.send})
		} else {
			// 事件流与在场推送并行；任一结束则两者都结束。
			pctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); c.pushPresence(pctx, sid) }()
			err = src.Stream(pctx, st, after, sink{send: c.send})
			cancel()
			<-done
		}
	}
	switch {
	case ctx.Err() != nil:
		return ""
	case errors.Is(err, feed.ErrDeleted):
		return "deleted"
	}
	h.log().Warn("subscription failed", "session", sid, "err", err)
	return "error"
}

type sink struct {
	send func(*v1.GatewayMessage) error
}

func (k sink) Event(e *v1.Event) error {
	return k.send(&v1.GatewayMessage{Msg: &v1.GatewayMessage_Event{Event: e}})
}

func (k sink) Delta(d live.Delta) error {
	return k.send(&v1.GatewayMessage{Msg: &v1.GatewayMessage_Delta{Delta: &v1.LiveDelta{
		SessionId: d.SessionID, RunId: d.RunID, Attempt: d.Attempt, AfterSeq: d.AfterSeq,
		Text: d.Text, Snapshot: d.Snapshot, End: d.End,
	}}})
}

// 连接层有自己的保活。
func (sink) Ping() error  { return nil }
func (sink) Flush() error { return nil }

// load 读取 Session 并要求它属于连接的 EndUser；否则一律视为不存在。
func (h *Handler) load(ctx context.Context, id Identity, sid string) (*session.State, error) {
	st, err := h.Service.Load(ctx, sid)
	if err == nil && (st.Created.GetBusinessLine() != id.BusinessLine || st.Created.GetEndUser() != id.EndUser) {
		err = fmt.Errorf("%w: session %s", service.ErrNotFound, sid)
	}
	return st, err
}

// Request 执行一个客户端请求。
func (h *Handler) Request(ctx context.Context, id Identity, req *v1.ClientRequest) *v1.ClientResponse {
	resp := &v1.ClientResponse{RequestId: req.GetRequestId()}
	if err := h.do(ctx, id, req, resp); err != nil {
		resp.Error = h.clientError(err)
	}
	return resp
}

func (h *Handler) do(ctx context.Context, id Identity, req *v1.ClientRequest, resp *v1.ClientResponse) error {
	by := "end_user:" + id.EndUser
	switch op := req.GetOp().(type) {
	case *v1.ClientRequest_CreateSession:
		sid, err := h.Service.Create(ctx, service.CreateRequest{BusinessLine: id.BusinessLine, EndUser: id.EndUser,
			Agent: op.CreateSession.GetAgent(), AgentVersion: op.CreateSession.GetAgentVersion()})
		resp.SessionId = sid
		return err
	case *v1.ClientRequest_Submit:
		if _, err := h.load(ctx, id, op.Submit.GetSessionId()); err != nil {
			return err
		}
		res, err := h.Service.SubmitWithID(ctx, op.Submit.GetSessionId(), op.Submit.GetInputId(), op.Submit.GetInput())
		if err != nil {
			return err
		}
		resp.RunId, resp.Steered, resp.Answered, resp.Duplicate = res.RunID, res.Steered, res.Answered, res.Duplicate
		return nil
	case *v1.ClientRequest_Interrupt:
		if _, err := h.load(ctx, id, op.Interrupt.GetSessionId()); err != nil {
			return err
		}
		return h.Service.Interrupt(ctx, op.Interrupt.GetSessionId(), op.Interrupt.GetRunId())
	case *v1.ClientRequest_Decide:
		if _, err := h.load(ctx, id, op.Decide.GetSessionId()); err != nil {
			return err
		}
		return h.Service.Decide(ctx, op.Decide.GetSessionId(), op.Decide.GetCallId(), op.Decide.GetApprove(), by)
	case *v1.ClientRequest_Answer:
		if _, err := h.load(ctx, id, op.Answer.GetSessionId()); err != nil {
			return err
		}
		return h.Service.Answer(ctx, op.Answer.GetSessionId(), op.Answer.GetCallId(), &askuser.Answer{
			Selected: op.Answer.GetSelected(), Values: op.Answer.GetValues(), Text: op.Answer.GetText()})
	case *v1.ClientRequest_Close:
		if _, err := h.load(ctx, id, op.Close.GetSessionId()); err != nil {
			return err
		}
		return h.Service.Close(ctx, op.Close.GetSessionId(), by, op.Close.GetReason())
	}
	return fmt.Errorf("%w: unknown request", service.ErrInvalid)
}

// clientError 与 HTTP API 的状态码一一对应（httpapi.Server.fail）。
func (h *Handler) clientError(err error) *v1.ClientError {
	e := &v1.ClientError{Code: "internal", Message: err.Error()}
	var ex *usage.ExceededError
	switch {
	case errors.Is(err, service.ErrNotFound):
		e.Code = "not_found"
	case errors.Is(err, service.ErrInvalid):
		e.Code = "invalid"
	case errors.Is(err, service.ErrConflict):
		e.Code = "conflict"
	case errors.Is(err, moderation.ErrRejected):
		e.Code = "rejected"
	case errors.Is(err, moderation.ErrUnavailable):
		e.Code = "unavailable"
	case errors.As(err, &ex):
		e.Code, e.RetryAt = "quota_exceeded", timestamppb.New(ex.Period.ResetAt)
	case errors.Is(err, usage.ErrExceeded):
		e.Code = "quota_exceeded"
	}
	if e.Code == "internal" {
		h.log().Error("client request failed", "err", err)
		e.Message = "internal error"
	}
	return e
}

// join 在订阅通过授权后写入在场记录，此前收到的 Activity 一并生效。
func (c *Conn) join(ctx context.Context, s *subscription, sid string) error {
	c.mu.Lock()
	s.joined = true
	e := c.entry(s, sid)
	c.mu.Unlock()
	return c.h.Presence.Put(ctx, e)
}

// entry 由订阅的当前状态生成在场记录；调用方持有 c.mu。
func (c *Conn) entry(s *subscription, sid string) *presence.Entry {
	return &presence.Entry{SessionID: sid, ConnID: c.connID, DeviceID: c.id.DeviceID, Label: c.id.Label, Kind: c.id.Kind,
		Focused: s.focused, TypingUntil: s.typingUntil, Expires: c.h.now().Add(presence.TTL)}
}

// activity 更新本设备在一个已订阅 Session 中的状态；未订阅时忽略。
func (c *Conn) activity(a *v1.Activity) {
	if c.h.Presence == nil {
		return
	}
	sid := a.GetSessionId()
	c.mu.Lock()
	s := c.subs[sid]
	if s == nil {
		c.mu.Unlock()
		return
	}
	s.focused = a.GetFocused()
	s.typingUntil = time.Time{}
	if a.GetTyping() {
		s.typingUntil = c.h.now().Add(presence.TypingFor)
	}
	if !s.joined {
		c.mu.Unlock()
		return
	}
	e := c.entry(s, sid)
	c.mu.Unlock()
	if err := c.h.Presence.Put(c.ctx, e); err != nil && c.ctx.Err() == nil {
		c.h.log().Warn("presence update failed", "session", sid, "err", err)
	}
}

// pushPresence 在订阅期间推送在场列表：先发一次，之后在记录变化或有记录过期时重发（内容不变则不发）。
func (c *Conn) pushPresence(ctx context.Context, sid string) {
	store := c.h.Presence.Store
	var last []*v1.Viewer
	sent := false
	for ctx.Err() == nil {
		now := c.h.now()
		entries, version, err := store.List(ctx, sid, now)
		if err != nil {
			if ctx.Err() == nil {
				c.h.log().Warn("presence list failed", "session", sid, "err", err)
			}
			if !sleep(ctx, time.Second) {
				return
			}
			continue
		}
		viewers := presence.Viewers(entries, now)
		if !sent || !sameViewers(last, viewers) {
			if c.send(&v1.GatewayMessage{Msg: &v1.GatewayMessage_Presence{Presence: &v1.Presence{SessionId: sid, Viewers: viewers}}}) != nil {
				return
			}
			last, sent = viewers, true
		}
		wctx, cancel := ctx, context.CancelFunc(func() {})
		if next := presence.NextChange(entries, now); !next.IsZero() {
			wctx, cancel = context.WithDeadline(ctx, next)
		}
		_ = store.Wait(wctx, sid, version)
		cancel()
	}
}

func sameViewers(a, b []*v1.Viewer) bool {
	return slices.EqualFunc(a, b, func(x, y *v1.Viewer) bool { return proto.Equal(x, y) })
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
