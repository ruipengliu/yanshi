package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/notify"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

// Hub 是网关的传输无关核心：所有方法同步执行，WebSocket 网关与模拟测试都通过它工作。
type Hub struct {
	Dir   Directory
	Inbox Inbox
	Store *session.Store
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
}

// Dispatch 把一次路由调用放入目标 Node 的 Inbox；Node 离线时尝试唤醒。
func (h *Hub) Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke) error {
	if err := h.Inbox.Put(ctx, nodeID, inv); err != nil {
		return err
	}
	if gone, err := Withdrawn(ctx, h.Deletions, inv.GetSessionId()); err != nil || gone {
		if gone {
			return h.Inbox.Remove(ctx, nodeID, inv.GetCallId())
		}
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
	class, err := h.commitResult(ctx, inv, res)
	if err != nil {
		return err
	}
	if err := h.Inbox.Remove(ctx, nodeID, inv.GetCallId()); err != nil {
		return err
	}
	return h.Queue.Enqueue(ctx, inv.GetSessionId(), class)
}

// commitResult 写入结果，并返回 Session 此后的工作类别（ADR-0029）。
func (h *Hub) commitResult(ctx context.Context, inv *v1.Invoke, res *v1.InvokeResult) (workqueue.Class, error) {
	st, err := h.Store.Load(ctx, inv.GetSessionId())
	if err != nil {
		return workqueue.Class{}, err
	}
	for range 16 {
		e := &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{
			RunId: inv.GetRunId(), CallId: inv.GetCallId(), Content: res.GetContent(), IsError: res.GetIsError(),
		}}}
		err := h.Store.Commit(ctx, st, e)
		if errors.Is(err, eventlog.ErrConflict) {
			if err := h.Store.SyncAfterConflict(ctx, st); err != nil {
				if errors.Is(err, session.ErrGone) {
					h.log().Info("node result dropped", "session", inv.GetSessionId(), "call", inv.GetCallId(), "reason", "session deleted")
					return st.WorkClass(), nil
				}
				return workqueue.Class{}, err
			}
			continue
		}
		if errors.Is(err, session.ErrInvalid) {
			// 校验失败：Run 已终态或调用已完成（超时、重复结果），结果作废。
			h.log().Info("node result dropped", "session", inv.GetSessionId(), "call", inv.GetCallId(), "reason", err)
			return st.WorkClass(), nil
		}
		// 存储故障：结果没有写入。返回错误，调用方不确认、不撤下 Inbox，重连后设备从账本重新返回结果。
		return st.WorkClass(), err
	}
	return workqueue.Class{}, fmt.Errorf("commit result for call %s: too many conflicts", inv.GetCallId())
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
