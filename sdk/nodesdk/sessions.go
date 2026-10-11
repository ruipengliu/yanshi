package nodesdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

// 会话客户端（docs/design/m3-duplex-channel.md）：与 Node 共用同一条连接（ADR-0024），
// 订阅 Session 的事件流并提交输入、审批等。断线重连后订阅自动按最后收到的 seq 续传。

// SessionHandler 接收一个订阅的消息。回调在连接的接收 goroutine 中按到达顺序串行调用，不应阻塞。
type SessionHandler interface {
	OnEvent(e *v1.Event)
	// OnDelta 收到模型输出的实时增量：Snapshot 为 true 时替换当前草稿，End 时丢弃未提交的草稿。
	OnDelta(d *v1.LiveDelta)
	// OnEnded 在网关结束订阅时调用（原因见 v1.SubscriptionEnded），之后不再回调。
	OnEnded(reason string)
}

// PresenceHandler 是 SessionHandler 的可选接口：接收在场设备列表（订阅后先收到一次，之后每次变化时收到全量）。
type PresenceHandler interface {
	OnPresence(p *v1.Presence)
}

// ErrDisconnected 表示请求已发出，但连接在收到响应之前断开：结果未知。
// 提交输入不是幂等的，应先从订阅中确认输入是否已生效，再决定是否重试。
var ErrDisconnected = errors.New("nodesdk: connection lost before response; outcome unknown")

// RequestError 是网关拒绝请求的原因，Code 见 v1.ClientError。
type RequestError struct {
	Code    string
	Message string
	// RetryAt 在 Code 为 "quota_exceeded" 时是配额重置时间。
	RetryAt time.Time
}

func (e *RequestError) Error() string { return e.Code + ": " + e.Message }

type subscriber struct {
	after uint64
	h     SessionHandler
	// focused 是最近一次 SetActivity 报告的前台状态，重连后随订阅重发（"正在输入"是瞬时的，不重发）。
	focused bool
}

type sessions struct {
	mu sync.Mutex
	// send 为 nil 表示当前未连接；ready 在连接建立时关闭，断开时换新。
	send    func(context.Context, *v1.NodeMessage) error
	ready   chan struct{}
	subs    map[string]*subscriber
	pending map[string]chan *v1.ClientResponse
	nextID  uint64
}

func (s *sessions) init() {
	s.ready = make(chan struct{})
	s.subs = map[string]*subscriber{}
	s.pending = map[string]chan *v1.ClientResponse{}
}

// attach 在连接建立后调用：重新订阅全部 Session，并放行等待连接的请求。
func (s *sessions) attach(send func(context.Context, *v1.NodeMessage) error) error {
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	for sid, sub := range s.subs {
		if err := send(ctx, subscribeMsg(sid, sub.after)); err != nil {
			return err
		}
		if sub.focused {
			if err := send(ctx, activityMsg(sid, true, false)); err != nil {
				return err
			}
		}
	}
	s.send = send
	close(s.ready)
	return nil
}

// detach 在连接断开时调用：未收到响应的请求以 ErrDisconnected 结束。
func (s *sessions) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.send = nil
	s.ready = make(chan struct{})
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
}

func activityMsg(sid string, focused, typing bool) *v1.NodeMessage {
	return &v1.NodeMessage{Msg: &v1.NodeMessage_Activity{Activity: &v1.Activity{SessionId: sid, Focused: focused, Typing: typing}}}
}

func subscribeMsg(sid string, after uint64) *v1.NodeMessage {
	return &v1.NodeMessage{Msg: &v1.NodeMessage_Subscribe{Subscribe: &v1.Subscribe{SessionId: sid, AfterSeq: after}}}
}

// handle 处理会话客户端消息；其他消息返回 false。
func (s *sessions) handle(m *v1.GatewayMessage) bool {
	switch m := m.GetMsg().(type) {
	case *v1.GatewayMessage_Event:
		s.mu.Lock()
		sub := s.subs[m.Event.GetSessionId()]
		// 重新订阅与旧订阅的残留消息可能重叠：只接受比已收到的更新的事件。
		fresh := sub != nil && m.Event.GetSeq() > sub.after
		if fresh {
			sub.after = m.Event.GetSeq()
		}
		s.mu.Unlock()
		if fresh {
			sub.h.OnEvent(m.Event)
		}
	case *v1.GatewayMessage_Delta:
		s.mu.Lock()
		sub := s.subs[m.Delta.GetSessionId()]
		s.mu.Unlock()
		if sub != nil {
			sub.h.OnDelta(m.Delta)
		}
	case *v1.GatewayMessage_Presence:
		s.mu.Lock()
		sub := s.subs[m.Presence.GetSessionId()]
		s.mu.Unlock()
		if ph, ok := sub.handler().(PresenceHandler); ok {
			ph.OnPresence(m.Presence)
		}
	case *v1.GatewayMessage_SubscriptionEnded:
		s.mu.Lock()
		sub := s.subs[m.SubscriptionEnded.GetSessionId()]
		delete(s.subs, m.SubscriptionEnded.GetSessionId())
		s.mu.Unlock()
		if sub != nil {
			sub.h.OnEnded(m.SubscriptionEnded.GetReason())
		}
	case *v1.GatewayMessage_Response:
		s.mu.Lock()
		ch := s.pending[m.Response.GetRequestId()]
		delete(s.pending, m.Response.GetRequestId())
		s.mu.Unlock()
		if ch != nil {
			ch <- m.Response
		}
	default:
		return false
	}
	return true
}

// Subscribe 订阅 Session 中 seq 大于 after 的事件与之后的实时增量，返回取消函数。
// 未连接时在连接建立后自动订阅；同一 Session 重复订阅时以新的为准。
func (c *Client) Subscribe(sessionID string, after uint64, h SessionHandler) (cancel func()) {
	s := &c.sessions
	sub := &subscriber{after: after, h: h}
	s.mu.Lock()
	s.subs[sessionID] = sub
	if s.send != nil {
		_ = s.send(context.Background(), subscribeMsg(sessionID, after))
	}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.subs[sessionID] != sub {
			return
		}
		delete(s.subs, sessionID)
		if s.send != nil {
			_ = s.send(context.Background(), &v1.NodeMessage{Msg: &v1.NodeMessage_Unsubscribe{Unsubscribe: &v1.Unsubscribe{SessionId: sessionID}}})
		}
	}
}

func (sub *subscriber) handler() SessionHandler {
	if sub == nil {
		return nil
	}
	return sub.h
}

// SetActivity 报告本设备在一个已订阅 Session 中的状态：focused 表示该 Session 正显示在前台，
// typing 表示正在输入（网关 10 秒后视为停止，持续输入时每隔几秒重发）。未订阅的 Session 忽略。
// 未连接时只记录前台状态，重连后随订阅重发。
func (c *Client) SetActivity(sessionID string, focused, typing bool) {
	s := &c.sessions
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[sessionID]
	if sub == nil {
		return
	}
	sub.focused = focused
	if s.send != nil {
		_ = s.send(context.Background(), activityMsg(sessionID, focused, typing))
	}
}

// Request 发出一个客户端请求并等待响应；未连接时等待连接建立。request_id 由 SDK 填写。
// ctx 结束时立即返回，包括写入卡住时（此时连接随之关闭、重连）；ctx 已结束的请求不发出。
func (c *Client) Request(ctx context.Context, req *v1.ClientRequest) (*v1.ClientResponse, error) {
	s := &c.sessions
	var ch chan *v1.ClientResponse
	for ch == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.send == nil {
			ready := s.ready
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
			}
			continue
		}
		s.nextID++
		req.RequestId = "r" + strconv.FormatUint(s.nextID, 10)
		ch = make(chan *v1.ClientResponse, 1)
		s.pending[req.RequestId] = ch
		send := s.send
		s.mu.Unlock()
		// 在锁外写：写入可能阻塞，持锁会挡住响应的分发、订阅变更与断线清理。
		if err := send(ctx, &v1.NodeMessage{Msg: &v1.NodeMessage_Request{Request: req}}); err != nil {
			s.mu.Lock()
			delete(s.pending, req.RequestId)
			s.mu.Unlock()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// 写入失败时连接已断开，请求可能已经到达：与断开在响应之前相同，结果未知（提交输入会以同一 ID 重试）。
			return nil, fmt.Errorf("%w: %v", ErrDisconnected, err)
		}
	}
	select {
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, req.GetRequestId())
		s.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrDisconnected
		}
		if e := resp.GetError(); e != nil {
			re := &RequestError{Code: e.GetCode(), Message: e.GetMessage()}
			if e.GetRetryAt() != nil {
				re.RetryAt = e.GetRetryAt().AsTime()
			}
			return nil, re
		}
		return resp, nil
	}
}

// CreateSession 为连接的 EndUser 创建 Session；version 为空时按发布配置分流。
func (c *Client) CreateSession(ctx context.Context, agent, version string) (string, error) {
	resp, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_CreateSession{CreateSession: &v1.CreateSession{
		Agent: agent, AgentVersion: version}}})
	if err != nil {
		return "", err
	}
	return resp.GetSessionId(), nil
}

// Submit 提交输入：有活跃 Run 时作为插话（steered 为 true），否则开启新 Run。
//
// SDK 为每次提交生成输入 ID：连接在响应之前断开时，重连后以同一 ID 自动重试，直到得到结果或 ctx 结束。
// 网关按 ID 去重，因此输入恰好生效一次。
func (c *Client) Submit(ctx context.Context, sessionID string, input ...*v1.ContentBlock) (runID string, steered bool, err error) {
	resp, err := c.SubmitInput(ctx, &v1.SubmitInput{SessionId: sessionID, Input: input, InputId: NewInputID()})
	if err != nil {
		return "", false, err
	}
	return resp.GetRunId(), resp.GetSteered(), nil
}

// SubmitInput 提交输入并返回完整的响应（answered、duplicate 等）。in.InputId 为空时不去重、断线不重试；
// 需要在进程重启后继续重试的 HostApp 应自己生成并持久保存 ID。
func (c *Client) SubmitInput(ctx context.Context, in *v1.SubmitInput) (*v1.ClientResponse, error) {
	for {
		resp, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_Submit{Submit: in}})
		if errors.Is(err, ErrDisconnected) && in.GetInputId() != "" {
			continue // Request 会等到重新连接
		}
		return resp, err
	}
}

// NewInputID 生成一个随机的输入 ID。
func NewInputID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "in_" + hex.EncodeToString(b[:])
}

// UIContext 返回界面上下文内容块，与用户的话一并提交（Submit），帮助模型理解"这个""这里"指什么
// （docs/design/m3-duplex-channel.md §8）。各字段有长度上限，超出时提交被拒绝（invalid）。
func UIContext(screen, ref, selection, content string) *v1.ContentBlock {
	return &v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{
		Screen: screen, Ref: ref, Selection: selection, Content: content}}}
}

// SubmitText 提交一段文本输入。
func (c *Client) SubmitText(ctx context.Context, sessionID, text string) (runID string, steered bool, err error) {
	return c.Submit(ctx, sessionID, &v1.ContentBlock{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: text}}})
}

func (c *Client) Interrupt(ctx context.Context, sessionID, runID string) error {
	_, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_Interrupt{Interrupt: &v1.InterruptRun{
		SessionId: sessionID, RunId: runID}}})
	return err
}

func (c *Client) Decide(ctx context.Context, sessionID, callID string, approve bool) error {
	_, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_Decide{Decide: &v1.DecideApproval{
		SessionId: sessionID, CallId: callID, Approve: approve}}})
	return err
}

// Answer 回答 Agent 的 ask_user 提问：selected 是点选的选项 ID，values 是填写的字段，text 是输入的文字，
// 可以只给出其中一部分。提问的内容见 AssistantMessage 中该调用的参数。
func (c *Client) Answer(ctx context.Context, sessionID, callID string, selected []string, values map[string]string, text string) error {
	_, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_Answer{Answer: &v1.AnswerQuestion{
		SessionId: sessionID, CallId: callID, Selected: selected, Values: values, Text: text}}})
	return err
}

func (c *Client) CloseSession(ctx context.Context, sessionID, reason string) error {
	_, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_Close{Close: &v1.CloseSession{
		SessionId: sessionID, Reason: reason}}})
	return err
}
