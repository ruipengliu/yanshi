package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/notify"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

// Hub 是网关的传输无关核心：所有方法同步执行，WebSocket 网关与模拟测试都通过它工作。
type Hub struct {
	Dir   Directory
	Inbox Inbox
	// Store 须配置 Waking：Node 结果在写日志的同一事务中唤醒 Session。
	Store *session.Store
	// Results 非 nil 时由它写入 Node 结果（service.Service.NodeResult）：调用已转入后台时，结果是一条输入而不是调用的结果
	// （ADR-0035），开启新 Run 须遵循与提交输入相同的规则。为 nil 时只写入 Run 中等待的调用的结果。
	Results ResultWriter
	// Queue 非 nil 时，Node 下线后唤醒 Run 中有调用在等它的 Session（ADR-0035）：Worker 据此在宽限期之后把调用转入后台，
	// 而不是等到调用的截止时间。
	Queue workqueue.Queue
	Auth  Authenticator
	Waker Waker
	// Deletions 非 nil 时，派发后复查删除记录，撤回属于已删除 Session 的调用（ADR-0015）。
	Deletions lifecycle.Deletions
	// Push 非 nil 时，Hello 中带推送令牌的连接登记为推送设备（docs/design/m3-duplex-channel.md §9）。
	Push   notify.Registry
	Clock  clock.Clock
	Logger *slog.Logger
}

// registerPush 登记连接的推送令牌（如有）。
func (h *Hub) registerPush(ctx context.Context, hello *v1.Hello, id Identity, label string) error {
	if h.Push == nil || hello.GetPushToken() == "" {
		return nil
	}
	now := time.Now()
	if h.Clock != nil {
		now = h.Clock.Now()
	}
	return h.Push.Register(ctx, &notify.Device{BusinessLine: id.BusinessLine, EndUser: id.EndUser, DeviceID: hello.GetNodeId(),
		Label: label, Kind: hello.GetKind(), Platform: hello.GetPushPlatform(), Token: hello.GetPushToken()}, now)
}

// Withdrawn 报告刚写入的、属于 sessionID 的数据是否应撤回：Session 已有删除记录。
// 写入方"先写、后查"，Janitor"先记、后清"，二者无论如何交错，总有一方会清除它（ADR-0015）。
func Withdrawn(ctx context.Context, d lifecycle.Deletions, sessionID string) (bool, error) {
	if d == nil || sessionID == "" {
		return false, nil
	}
	return lifecycle.Deleted(ctx, d, sessionID)
}

// ResultWriter 写入设备返回的路由调用结果，并在同一事务中唤醒 Session。结果作废（调用已不在等待）时返回
// session.ErrInvalid，Session 已删除时返回 session.ErrGone。
type ResultWriter interface {
	NodeResult(ctx context.Context, sessionID, runID, callID string, content []*v1.ContentBlock, isErr bool) error
}

// Conn 是一条已认证的 Node 连接。
type Conn struct {
	NodeID       string
	Label        string
	BusinessLine string
	EndUser      string
	// Expires 是接入凭证的到期时间，网关在此刻断开；零值表示不过期。
	Expires time.Time
	// ClientOnly 表示只作会话客户端，未登记为 Node。
	ClientOnly bool
	gen        uint64
}

func (h *Hub) log() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// Connect 认证并注册 Node。Hello.client_only 时只认证、不登记为 Node（ADR-0024），返回的 Conn
// 不能用于 Disconnect。
func (h *Hub) Connect(ctx context.Context, hello *v1.Hello) (*Conn, error) {
	if hello.GetNodeId() == "" {
		return nil, errors.New("hello: node_id is required")
	}
	if Reserved(hello.GetNodeId()) {
		return nil, fmt.Errorf("hello: node_id %q is reserved", hello.GetNodeId())
	}
	if hello.GetClientOnly() {
		if len(hello.GetCapabilities()) > 0 {
			return nil, errors.New("hello: a client-only connection cannot declare capabilities")
		}
		id, err := h.Auth.Authenticate(ctx, hello)
		if err != nil {
			return nil, fmt.Errorf("authenticate: %w", err)
		}
		label := SanitizeLabel(hello.GetLabel())
		if err := h.registerPush(ctx, hello, id, label); err != nil {
			return nil, err
		}
		return &Conn{NodeID: hello.GetNodeId(), Label: label, BusinessLine: id.BusinessLine,
			EndUser: id.EndUser, Expires: id.Expires, ClientOnly: true}, nil
	}
	id, err := h.Auth.Authenticate(ctx, hello)
	if err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	label, gen, err := h.Dir.Register(ctx, Info{
		NodeID: hello.GetNodeId(), Scope: Scope{BusinessLine: id.BusinessLine, EndUser: id.EndUser}, Label: hello.GetLabel(), Kind: hello.GetKind(),
		HostApp: hello.GetHostApp(), Capabilities: hello.GetCapabilities(),
	})
	if err != nil {
		return nil, err
	}
	if err := h.registerPush(ctx, hello, id, label); err != nil {
		return nil, err
	}
	h.log().Info("node connected", "node", hello.GetNodeId(), "label", label, "capabilities", len(hello.GetCapabilities()))
	return &Conn{NodeID: hello.GetNodeId(), Label: label, BusinessLine: id.BusinessLine, EndUser: id.EndUser,
		Expires: id.Expires, gen: gen}, nil
}

func (h *Hub) Disconnect(ctx context.Context, c *Conn) {
	_ = h.Dir.SetOffline(ctx, c.NodeID, c.gen)
	h.log().Info("node disconnected", "node", c.NodeID)
	if err := h.wakeWaiting(ctx, c.NodeID); err != nil {
		h.log().Warn("wake on disconnect failed", "node", c.NodeID, "err", err)
	}
}

// wakeWaiting 唤醒 Run 中有调用在等 nodeID 的 Session（ADR-0035）：调用在设备在线时派发，Run 挂起到调用的截止时间；设备
// 执行中途下线时，若不唤醒，Run 要等到截止时间才发现。醒来的 Worker 看到设备离线：未满宽限期就停放到宽限期结束，满了就
// 转入后台。已转入后台的调用不必唤醒：它的 Session 停放到后台截止时间，设备下线不改变什么。尽力而为：唤醒失败时
// Run 仍会在截止时间醒来。
func (h *Hub) wakeWaiting(ctx context.Context, nodeID string) error {
	if h.Queue == nil {
		return nil
	}
	pending, _, err := h.Inbox.Pending(ctx, nodeID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, inv := range pending {
		sid := inv.GetSessionId()
		if sid == "" || seen[sid] {
			continue
		}
		seen[sid] = true
		st, err := h.Store.Load(ctx, sid)
		if err != nil {
			return err
		}
		if st.Created == nil || st.Closed != nil || !awaits(st.Active(), nodeID) {
			continue
		}
		if err := h.Queue.Enqueue(ctx, sid, st.WorkClass()); err != nil {
			return err
		}
		// 队列中的项属于 Session 数据：入队之后复查删除记录（ADR-0015）。
		if _, err := lifecycle.Recheck(ctx, h.Deletions, sid, func(ctx context.Context) error { return h.Queue.Remove(ctx, sid) }); err != nil {
			return err
		}
		metrics.DisconnectWakes.WithLabelValues().Inc()
		h.log().Info("session woken on node disconnect", "node", nodeID, "session", sid)
	}
	return nil
}

// awaits 报告 Run 中是否有已派发、尚无结果、路由到 nodeID 的调用。
func awaits(r *session.Run, nodeID string) bool {
	if r == nil {
		return false
	}
	for _, c := range r.Calls {
		if c.Dispatched() && c.NodeID == nodeID {
			return true
		}
	}
	return false
}

// Dispatch 把一次路由调用放入目标 Node 的 Inbox；Node 离线时尝试唤醒。
func (h *Hub) Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke) error {
	if err := h.Inbox.Put(ctx, nodeID, inv); err != nil {
		return err
	}
	if gone, err := lifecycle.Recheck(ctx, h.Deletions, inv.GetSessionId(), func(ctx context.Context) error {
		return h.Inbox.Remove(ctx, nodeID, inv.GetCallId())
	}); err != nil || gone {
		return err
	}
	info, err := h.Dir.Get(ctx, nodeID)
	if err != nil {
		return err
	}
	if !info.Online && h.Waker != nil {
		h.Waker.Wake(ctx, info)
	}
	return nil
}

// Cancel 撤回尚未完成的路由调用；已投递的由网关通知 Node 取消。
func (h *Hub) Cancel(ctx context.Context, nodeID, callID string) error {
	return h.Inbox.Remove(ctx, nodeID, callID)
}

// Result 把 Node 返回的结果写入日志并唤醒 Session。
// 对已完成、已取消或不属于该 Node 的调用静默忽略：调用方应总是向 Node 确认（ResultAck）。
func (h *Hub) Result(ctx context.Context, nodeID string, res *v1.InvokeResult) error {
	pending, _, err := h.Inbox.Pending(ctx, nodeID)
	if err != nil {
		return err
	}
	var inv *v1.Invoke
	for _, p := range pending {
		if p.GetCallId() == res.GetCallId() {
			inv = p
		}
	}
	if inv == nil {
		return nil
	}
	if inv.GetSessionId() == "" {
		// 不属于任何 Session 的平台调用（如清除本地记录），没有结果要写入日志。
		return h.Inbox.Remove(ctx, nodeID, inv.GetCallId())
	}
	if err := h.commitResult(ctx, inv, res); err != nil {
		return err
	}
	return h.Inbox.Remove(ctx, nodeID, inv.GetCallId())
}

// commitResult 写入结果，并在同一事务中唤醒 Session（CommitAndWake）：写入之后、撤下 Inbox 之前崩溃时，
// 设备重发的结果因调用已完成而作废，Session 已经在队列中。
func (h *Hub) commitResult(ctx context.Context, inv *v1.Invoke, res *v1.InvokeResult) error {
	var err error
	if h.Results != nil {
		err = h.Results.NodeResult(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), res.GetContent(), res.GetIsError())
	} else {
		var st *session.State
		if st, err = h.Store.Load(ctx, inv.GetSessionId()); err != nil {
			return err
		}
		err = h.Store.Update(ctx, st, func(*session.State) (session.Decision, error) {
			e := session.ExternalResult(inv.GetRunId(), inv.GetCallId(), res.GetContent(), res.GetIsError())
			return session.Decision{Events: []*v1.Event{e}, Wake: true}, nil
		})
	}
	switch {
	case errors.Is(err, session.ErrGone):
		h.log().Info("node result dropped", "session", inv.GetSessionId(), "call", inv.GetCallId(), "reason", "session deleted")
		return nil
	case errors.Is(err, session.ErrInvalid):
		// 校验失败：Run 已终态或调用已完成（超时、重复结果），或后台调用已了结（到期、取消），结果作废。
		h.log().Info("node result dropped", "session", inv.GetSessionId(), "call", inv.GetCallId(), "reason", err)
		return nil
	}
	// 存储故障：结果没有写入，或写入了而回包丢失。返回错误，调用方不确认、不撤下 Inbox，重连后设备从账本
	// 重新返回结果：没写入的这次写入，写入了的这次作废。
	return err
}

// LogWaker 只记录唤醒请求；真实推送通道见 ADR-0001。
type LogWaker struct{ Logger *slog.Logger }

func (w LogWaker) Wake(_ context.Context, info *Info) {
	if w.Logger != nil {
		w.Logger.Info("wake offline node (push stub)", "node", info.NodeID, "label", info.Label, "host_app", info.HostApp)
	}
}

// DeleteEndUser 删除 EndUser 的全部 Node 登记（注销账号，ADR-0015）。
func (h *Hub) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	_, err := h.Dir.DeleteScope(ctx, Scope{BusinessLine: businessLine, EndUser: endUser})
	return err
}
