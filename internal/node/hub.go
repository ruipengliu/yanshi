package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

// Hub 是网关的传输无关核心：所有方法同步执行，WebSocket 网关与模拟测试都通过它工作。
type Hub struct {
	Dir    Directory
	Inbox  Inbox
	Store  *session.Store
	Queue  workqueue.Queue
	Auth   Authenticator
	Waker  Waker
	Logger *slog.Logger
}

// Conn 是一条已认证的 Node 连接。
type Conn struct {
	NodeID string
	Label  string
	// Expires 是接入凭证的到期时间，网关在此刻断开；零值表示不过期。
	Expires time.Time
	gen     uint64
}

func (h *Hub) log() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// Connect 认证并注册 Node。
func (h *Hub) Connect(ctx context.Context, hello *v1.Hello) (*Conn, error) {
	if hello.GetNodeId() == "" {
		return nil, errors.New("hello: node_id is required")
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
	h.log().Info("node connected", "node", hello.GetNodeId(), "label", label, "capabilities", len(hello.GetCapabilities()))
	return &Conn{NodeID: hello.GetNodeId(), Label: label, Expires: id.Expires, gen: gen}, nil
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
	if err := h.commitResult(ctx, inv, res); err != nil {
		return err
	}
	if err := h.Inbox.Remove(ctx, nodeID, inv.GetCallId()); err != nil {
		return err
	}
	return h.Queue.Enqueue(ctx, inv.GetSessionId())
}

func (h *Hub) commitResult(ctx context.Context, inv *v1.Invoke, res *v1.InvokeResult) error {
	st, err := h.Store.Load(ctx, inv.GetSessionId())
	if err != nil {
		return err
	}
	for range 16 {
		e := &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{
			RunId: inv.GetRunId(), CallId: inv.GetCallId(), Content: res.GetContent(), IsError: res.GetIsError(),
		}}}
		err := h.Store.Commit(ctx, st, e)
		if errors.Is(err, eventlog.ErrConflict) {
			if err := h.Store.Sync(ctx, st); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			// 校验失败：Run 已终态或调用已完成（超时、重复结果），结果作废。
			h.log().Info("node result dropped", "session", inv.GetSessionId(), "call", inv.GetCallId(), "reason", err)
		}
		return nil
	}
	return fmt.Errorf("commit result for call %s: too many conflicts", inv.GetCallId())
}

// LogWaker 只记录唤醒请求；真实推送通道见 ADR-0001。
type LogWaker struct{ Logger *slog.Logger }

func (w LogWaker) Wake(_ context.Context, info *Info) {
	if w.Logger != nil {
		w.Logger.Info("wake offline node (push stub)", "node", info.NodeID, "label", info.Label, "host_app", info.HostApp)
	}
}
