// Package wsgateway 以 WebSocket 承载 Node 协议，业务逻辑全部委托给 node.Hub。
package wsgateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/node"
)

type Gateway struct {
	Hub    *node.Hub
	Logger *slog.Logger
	// PingInterval 是心跳间隔；对端无响应时断开，Node 随之标记离线。
	PingInterval time.Duration
}

func (g *Gateway) log() *slog.Logger {
	if g.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return g.Logger
}

type conn struct {
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (c *conn) send(ctx context.Context, m *v1.GatewayMessage) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageBinary, b)
}

func (c *conn) read(ctx context.Context) (*v1.NodeMessage, error) {
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	m := &v1.NodeMessage{}
	return m, proto.Unmarshal(b, m)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)
	c := &conn{ws: ws}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	first, err := c.read(hctx)
	hcancel()
	if err != nil || first.GetHello() == nil {
		ws.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	nc, err := g.Hub.Connect(ctx, first.GetHello())
	if err != nil {
		g.log().Warn("node rejected", "err", err)
		ws.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	// 断开时用独立 context 标记离线，避免请求 context 已取消导致漏标。
	defer g.Hub.Disconnect(context.WithoutCancel(ctx), nc)
	if err := c.send(ctx, &v1.GatewayMessage{Msg: &v1.GatewayMessage_Welcome{Welcome: &v1.Welcome{
		NodeId: nc.NodeID, Label: nc.Label,
	}}}); err != nil {
		return
	}

	errc := make(chan error, 3)
	go func() { errc <- g.deliver(ctx, c, nc.NodeID) }()
	go func() { errc <- g.receive(ctx, c, nc.NodeID) }()
	go func() { errc <- g.keepalive(ctx, ws) }()
	err = <-errc
	if err != nil && !errors.Is(err, context.Canceled) {
		g.log().Info("node connection closed", "node", nc.NodeID, "err", err)
	}
}

// deliver 把 Inbox 与已投递集合对齐：新增项发送 Invoke，被撤回的已投递项发送 Cancel。
func (g *Gateway) deliver(ctx context.Context, c *conn, nodeID string) error {
	sent := map[string]bool{}
	for {
		pending, version, err := g.Hub.Inbox.Pending(ctx, nodeID)
		if err != nil {
			return err
		}
		live := map[string]bool{}
		for _, inv := range pending {
			live[inv.GetCallId()] = true
			if sent[inv.GetCallId()] {
				continue
			}
			if err := c.send(ctx, &v1.GatewayMessage{Msg: &v1.GatewayMessage_Invoke{Invoke: inv}}); err != nil {
				return err
			}
			sent[inv.GetCallId()] = true
		}
		for id := range sent {
			if !live[id] {
				delete(sent, id)
				if err := c.send(ctx, &v1.GatewayMessage{Msg: &v1.GatewayMessage_Cancel{Cancel: &v1.Cancel{CallId: id}}}); err != nil {
					return err
				}
			}
		}
		if err := g.Hub.Inbox.Wait(ctx, nodeID, version); err != nil {
			return err
		}
	}
}

func (g *Gateway) receive(ctx context.Context, c *conn, nodeID string) error {
	for {
		m, err := c.read(ctx)
		if err != nil {
			return err
		}
		res := m.GetResult()
		if res == nil {
			continue
		}
		if err := g.Hub.Result(ctx, nodeID, res); err != nil {
			// 未确认的结果会在重连后因重新投递而再次返回。
			return err
		}
		if err := c.send(ctx, &v1.GatewayMessage{Msg: &v1.GatewayMessage_ResultAck{ResultAck: &v1.ResultAck{CallId: res.GetCallId()}}}); err != nil {
			return err
		}
	}
}

func (g *Gateway) keepalive(ctx context.Context, ws *websocket.Conn) error {
	interval := g.PingInterval
	if interval == 0 {
		interval = 20 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, interval)
			err := ws.Ping(pctx)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}
