package nodesdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
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
	// after 是这个订阅者已收到的最后一个 seq：重新订阅与旧订阅的残留消息可能重叠，只交给它更新的事件。
	after uint64
	h     SessionHandler
}

// subscription 是一个 Session 在本连接上的订阅。Subscribe 的订阅者至多一个（新的取代旧的）；PendingTracker 对子 Session
// 的订阅者（observe）与它并存、各自取消，这样 HostApp 打开子 Session 时不会挤掉跟踪器的订阅，反之亦然。网关上一个 Session
// 只有一个订阅（重复订阅以新的 after_seq 重新开始），从各订阅者中最小的 after 续传，每个订阅者各自丢弃已收到的事件。
type subscription struct {
	primary   *subscriber
	observers []*subscriber
	// focused 是 Subscribe 的订阅者最近一次 SetActivity 报告的前台状态，重连后随订阅重发（"正在输入"是瞬时的，不重发）。
	focused bool
}

func (sub *subscription) members() []*subscriber {
	if sub == nil {
		return nil
	}
	out := make([]*subscriber, 0, 1+len(sub.observers))
	if sub.primary != nil {
		out = append(out, sub.primary)
	}
	return append(out, sub.observers...)
}

func (sub *subscription) has(n *subscriber) bool {
	return sub != nil && (sub.primary == n || slices.Contains(sub.observers, n))
}

func (sub *subscription) minAfter() uint64 {
	var after uint64
	for i, n := range sub.members() {
		if i == 0 || n.after < after {
			after = n.after
		}
	}
	return after
}

type sessions struct {
	mu sync.Mutex
	// send 为 nil 表示当前未连接；ready 在连接建立时关闭，断开时换新。
	send    func(context.Context, *v1.NodeMessage) error
	ready   chan struct{}
	subs    map[string]*subscription
	pending map[string]chan *v1.ClientResponse
	nextID  uint64
}

func (s *sessions) init() {
	s.ready = make(chan struct{})
	s.subs = map[string]*subscription{}
	s.pending = map[string]chan *v1.ClientResponse{}
}

// attach 在连接建立后调用：重新订阅全部 Session，并放行等待连接的请求。
func (s *sessions) attach(send func(context.Context, *v1.NodeMessage) error) error {
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	for sid, sub := range s.subs {
		if err := send(ctx, subscribeMsg(sid, sub.minAfter())); err != nil {
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
		sid, seq := m.Event.GetSessionId(), m.Event.GetSeq()
		s.mu.Lock()
		// 重新订阅与旧订阅的残留消息可能重叠：每个订阅者只接受比自己已收到的更新的事件。
		var fresh []*subscriber
		for _, n := range s.subs[sid].members() {
			if seq > n.after {
				n.after = seq
				fresh = append(fresh, n)
			}
		}
		s.mu.Unlock()
		for _, n := range fresh {
			if s.current(sid, n) {
				n.h.OnEvent(m.Event)
			}
		}
	case *v1.GatewayMessage_Delta:
		sid := m.Delta.GetSessionId()
		for _, n := range s.members(sid) {
			if s.current(sid, n) {
				n.h.OnDelta(m.Delta)
			}
		}
	case *v1.GatewayMessage_Presence:
		sid := m.Presence.GetSessionId()
		for _, n := range s.members(sid) {
			if ph, ok := n.h.(PresenceHandler); ok && s.current(sid, n) {
				ph.OnPresence(m.Presence)
			}
		}
	case *v1.GatewayMessage_SubscriptionEnded:
		sid := m.SubscriptionEnded.GetSessionId()
		s.mu.Lock()
		ended := s.subs[sid].members()
		delete(s.subs, sid)
		s.mu.Unlock()
		for _, n := range ended {
			n.h.OnEnded(m.SubscriptionEnded.GetReason())
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

func (s *sessions) members(sid string) []*subscriber {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs[sid].members()
}

// current 报告订阅者是否仍在订阅：前一个回调可能取消了同一 Session 的其他订阅者，已取消的不再回调。
func (s *sessions) current(sid string, n *subscriber) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs[sid].has(n)
}

// Subscribe 订阅 Session 中 seq 大于 after 的事件与之后的实时增量，返回取消函数。
// 未连接时在连接建立后自动订阅；同一 Session 重复订阅时以新的为准。
func (c *Client) Subscribe(sessionID string, after uint64, h SessionHandler) (cancel func()) {
	return c.sessions.add(sessionID, &subscriber{after: after, h: h}, true)
}

// observe 是 PendingTracker 对子 Session 的订阅：与 Subscribe 的订阅者并存，互不取代，各自取消。
func (c *Client) observe(sessionID string, after uint64, h SessionHandler) (cancel func()) {
	return c.sessions.add(sessionID, &subscriber{after: after, h: h}, false)
}

func (s *sessions) add(sid string, n *subscriber, primary bool) (cancel func()) {
	s.mu.Lock()
	sub := s.subs[sid]
	if sub == nil {
		sub = &subscription{}
		s.subs[sid] = sub
	}
	if primary {
		sub.primary = n
	} else {
		sub.observers = append(sub.observers, n)
	}
	// 新的订阅者可能需要比已收到的更早的事件：网关上重新订阅，从最小的 after 续传；其他订阅者丢弃重放的事件。
	if s.send != nil {
		_ = s.send(context.Background(), subscribeMsg(sid, sub.minAfter()))
	}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		sub := s.subs[sid]
		switch {
		case sub == nil:
			return
		case primary:
			if sub.primary != n {
				return
			}
			sub.primary = nil
		default:
			i := slices.Index(sub.observers, n)
			if i < 0 {
				return
			}
			sub.observers = slices.Delete(sub.observers, i, i+1)
		}
		s.release(sid, sub)
	}
}

// release 在一个订阅者取消后调用（持有 mu）：没有订阅者时退订；Subscribe 的订阅者走了而跟踪器还在时撤下前台状态，
// 否则网关以为用户仍在看这个 Session，不再推送它的提醒。
func (s *sessions) release(sid string, sub *subscription) {
	if sub.primary == nil && len(sub.observers) == 0 {
		delete(s.subs, sid)
		if s.send != nil {
			_ = s.send(context.Background(), &v1.NodeMessage{Msg: &v1.NodeMessage_Unsubscribe{Unsubscribe: &v1.Unsubscribe{SessionId: sid}}})
		}
		return
	}
	if sub.primary == nil && sub.focused {
		sub.focused = false
		if s.send != nil {
			_ = s.send(context.Background(), activityMsg(sid, false, false))
		}
	}
}

// SetActivity 报告本设备在一个已订阅 Session 中的状态：focused 表示该 Session 正显示在前台，
// typing 表示正在输入（网关 10 秒后视为停止，持续输入时每隔几秒重发）。未订阅的 Session 忽略。
// 未连接时只记录前台状态，重连后随订阅重发。
func (c *Client) SetActivity(sessionID string, focused, typing bool) {
	s := &c.sessions
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[sessionID]
	if sub == nil || sub.primary == nil {
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

// SubmitCaptures 提交一批旁听转写（docs/design/captures.md §3，ADR-0036）：佩戴外设的 EndUser 与他人的对话，不开启 Run。
// 只有开启了旁听的 AgentDef 的 Session 接收（否则 invalid）。in.CaptureId 必须由 HostApp 生成并随本地缓冲持久保存
// （NewCaptureID）：连接在响应之前断开时，以同一 ID 自动重试，直到得到结果或 ctx 结束；进程重启后补交同一批时也用原来的
// ID，网关按它去重（duplicate 为 true）。响应的 redacted 是内容安全拦截的段数；内容安全不可用时返回 unavailable，稍后以
// 同一 ID 重试。片段不能设置 redacted，拦截由平台判断。
func (c *Client) SubmitCaptures(ctx context.Context, in *v1.SubmitCaptures) (*v1.ClientResponse, error) {
	if in.GetCaptureId() == "" {
		return nil, errors.New("nodesdk: capture id is required")
	}
	for {
		resp, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_SubmitCaptures{SubmitCaptures: in}})
		if errors.Is(err, ErrDisconnected) {
			continue // Request 会等到重新连接
		}
		return resp, err
	}
}

// NewCaptureID 生成一个随机的旁听批次 ID（SubmitCaptures）。
func NewCaptureID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "cap_" + hex.EncodeToString(b[:])
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

// SubmitCardAction 提交用户在卡片上的动作（点按钮或点选列表项，docs/design/cards.md §4）：a 填 card_id、call_id、
// action、字段值与收到这一版卡片的事件序号（CardOf）。有进行中的 Run 时并入它，否则开启新 Run；同一版卡片上的同一动作
// 只生效一次（重复点击的响应 duplicate 为 true）。
func (c *Client) SubmitCardAction(ctx context.Context, sessionID string, a *v1.CardAction) (*v1.ClientResponse, error) {
	return c.SubmitInput(ctx, &v1.SubmitInput{SessionId: sessionID, InputId: NewInputID(),
		Input: []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: a}}}})
}

// CardOf 返回事件中的卡片（show_card 的结果）；没有时返回 nil。同一 card_id 的卡片以最新的一版为准。
func CardOf(e *v1.Event) *v1.Card {
	for _, b := range e.GetToolResult().GetContent() {
		if c := b.GetCard(); c != nil {
			return c
		}
	}
	return nil
}

// CardActionOf 返回事件记下的、已执行的卡片动作（平台核对、规范化后的，docs/design/cards.md §5）；没有时返回 nil。
// 卡片动作作为输入时在 RunRequested 或 Steered 的输入中；Run 正在等 ask_user 的回答时，它成为回答，记在 ToolResult 的
// card_action 中。客户端据此把 call_id 那一版卡片上的该动作显示为已选：三处都要看，否则作为回答的动作在各设备上都不显示，
// 再点却得到 duplicate。
func CardActionOf(e *v1.Event) *v1.CardAction {
	if a := e.GetToolResult().GetCardAction(); a != nil {
		return a
	}
	input := e.GetRunRequested().GetInput()
	if s := e.GetSteered(); s != nil {
		input = s.GetInput()
	}
	for _, b := range input {
		if a := b.GetCardAction(); a != nil {
			return a
		}
	}
	return nil
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

// CancelBackground 取消一个未了结的后台调用（ADR-0035）：设备上线后不再执行它，不开启新 Run，模型之后看到它已取消。
// 调用已了结（已有结果、已到期或已取消）时返回 conflict。未了结的后台调用见 BackgroundCalls。
func (c *Client) CancelBackground(ctx context.Context, sessionID, callID string) error {
	_, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_CancelBackground{CancelBackground: &v1.CancelBackground{
		SessionId: sessionID, CallId: callID}}})
	return err
}

func (c *Client) CloseSession(ctx context.Context, sessionID, reason string) error {
	_, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_Close{Close: &v1.CloseSession{
		SessionId: sessionID, Reason: reason}}})
	return err
}
