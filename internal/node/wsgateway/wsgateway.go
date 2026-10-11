// Package wsgateway 以 WebSocket 承载 Connection 协议：Node 角色委托给 node.Hub，
// 会话客户端角色委托给 channel（ADR-0024）。帧编码跟随 Hello：二进制帧为 protobuf，文本帧为 protojson。
package wsgateway

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/channel"
	"yanshi/internal/metrics"
	"yanshi/internal/node"
)

type Gateway struct {
	Hub *node.Hub
	// Channel 为 nil 时不支持会话客户端消息（只作 Node）。
	Channel *channel.Handler
	Logger  *slog.Logger
	// PingInterval 是心跳间隔；对端无响应时断开，Node 随之标记离线。
	PingInterval time.Duration
	// MaxHandshakes 是同时进行的接入握手（读 Hello、鉴权、登记 Node）上限，默认 128；HandshakeWait 是排队的上限，
	// 默认 2 秒，超过时以 503 拒绝，Retry-After 为带抖动的 2～10 秒。网关滚动重启时成批的连接同时重连，而登记 Node
	// 是一次数据库事务：不限制时重连风暴会压垮数据库（延展性评审 §4.5）。
	MaxHandshakes int
	HandshakeWait time.Duration

	once  sync.Once
	slots chan struct{}
}

// admit 取得一个握手名额；排队超过 HandshakeWait 时返回 false。
func (g *Gateway) admit(ctx context.Context) (func(), bool) {
	g.once.Do(func() {
		n := g.MaxHandshakes
		if n <= 0 {
			n = 128
		}
		g.slots = make(chan struct{}, n)
	})
	wait := g.HandshakeWait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case g.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.slots }) }, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

func (g *Gateway) log() *slog.Logger {
	if g.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return g.Logger
}

type conn struct {
	ws *websocket.Conn
	// text 为 true 时以 protojson 文本帧收发，由第一帧（Hello）的类型决定。
	text bool
	wmu  sync.Mutex
}

var pj = protojson.MarshalOptions{UseProtoNames: true}

func (c *conn) send(ctx context.Context, m *v1.GatewayMessage) error {
	typ, marshal := websocket.MessageBinary, proto.Marshal
	if c.text {
		typ, marshal = websocket.MessageText, pj.Marshal
	}
	b, err := marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(wctx, typ, b)
}

func (c *conn) read(ctx context.Context) (*v1.NodeMessage, error) {
	typ, b, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	m := &v1.NodeMessage{}
	if typ == websocket.MessageText {
		return m, protojson.Unmarshal(b, m)
	}
	return m, proto.Unmarshal(b, m)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handshakeDone, ok := g.admit(r.Context())
	if !ok {
		metrics.HandshakesRejected.WithLabelValues().Inc()
		w.Header().Set("Retry-After", strconv.Itoa(2+rand.IntN(9)))
		http.Error(w, "too many connection attempts; retry later", http.StatusServiceUnavailable)
		return
	}
	defer handshakeDone()
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
	typ, b, err := ws.Read(hctx)
	hcancel()
	first := &v1.NodeMessage{}
	if err == nil {
		c.text = typ == websocket.MessageText
		if c.text {
			err = protojson.Unmarshal(b, first)
		} else {
			err = proto.Unmarshal(b, first)
		}
	}
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
	if !nc.ClientOnly {
		// 断开时用独立 context 标记离线，避免请求 context 已取消导致漏标。
		defer g.Hub.Disconnect(context.WithoutCancel(ctx), nc)
		metrics.NodesConnected.Inc()
		defer metrics.NodesConnected.Dec()
	}
	if err := c.send(ctx, &v1.GatewayMessage{Msg: &v1.GatewayMessage_Welcome{Welcome: &v1.Welcome{
		NodeId: nc.NodeID, Label: nc.Label,
	}}}); err != nil {
		return
	}
	handshakeDone()
	var cc *channel.Conn
	if g.Channel != nil {
		cc = g.Channel.Open(ctx, channel.Identity{BusinessLine: nc.BusinessLine, EndUser: nc.EndUser,
			DeviceID: nc.NodeID, Label: nc.Label, Kind: first.GetHello().GetKind()},
			func(m *v1.GatewayMessage) error { return c.send(ctx, m) })
		// 先于 ws.CloseNow 执行（defer 后进先出）：等订阅与请求退出后再关闭连接。
		defer cc.Close()
		defer cancel()
	}

	errc := make(chan error, 4)
	if !nc.Expires.IsZero() {
		// 令牌到期即断开，SDK 取新令牌重连（docs/design/auth.md §4）。
		t := time.NewTimer(time.Until(nc.Expires))
		defer t.Stop()
		go func() {
			select {
			case <-t.C:
				ws.Close(websocket.StatusPolicyViolation, "token expired")
				errc <- errors.New("token expired")
			case <-ctx.Done():
			}
		}()
	}
	if !nc.ClientOnly {
		go func() { errc <- g.deliver(ctx, c, nc.NodeID) }()
	}
	go func() { errc <- g.receive(ctx, c, nc, cc) }()
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

func (g *Gateway) receive(ctx context.Context, c *conn, nc *node.Conn, cc *channel.Conn) error {
	for {
		m, err := c.read(ctx)
		if err != nil {
			return err
		}
		if cc != nil && cc.Handle(m) {
			continue
		}
		res := m.GetResult()
		if res == nil || nc.ClientOnly {
			continue
		}
		nodeID := nc.NodeID
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
