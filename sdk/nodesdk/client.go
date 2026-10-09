package nodesdk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

const Version = "0.1.0"

type Config struct {
	// URL 是网关地址，如 ws://127.0.0.1:8080/v1/nodes/connect。
	URL string
	// NodeID 必须在设备上持久保存，跨重启不变。
	NodeID string
	// TokenSource 返回业务线签发的用户令牌，每次连接时调用；令牌到期时网关断开连接，
	// SDK 重连并重新取令牌（docs/design/auth.md §4）。HostApp 通常向自己的业务线服务端换取。
	TokenSource TokenSource
	// Token 是固定令牌，仅在 TokenSource 为空时使用（开发）。
	Token        string
	BusinessLine string
	EndUser      string
	Label        string
	Kind         string
	HostApp      string
	Executor     *Executor
	Logger       *slog.Logger
	// OnConnected 在每次连接成功后调用，参数为网关分配的标签。
	OnConnected func(label string)
	// LedgerRetention 是账本记录的保留期，默认 DefaultLedgerRetention（7 天）；SDK 每小时清理一次过期记录。
	LedgerRetention time.Duration
}

type Client struct{ cfg Config }

func NewClient(cfg Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Client{cfg: cfg}
}

// Run 保持与网关的连接，断开后以指数退避重连，直到 ctx 结束；期间定期按保留期清理账本。
func (c *Client) Run(ctx context.Context) error {
	go c.pruneLoop(ctx)
	backoff := 500 * time.Millisecond
	for {
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			backoff = 500 * time.Millisecond
		}
		c.cfg.Logger.Warn("node connection lost", "err", err, "retry_in", backoff)
		jitter := time.Duration(rand.Int64N(int64(backoff / 2)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + jitter):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// session 处理一条连接直到断开；返回是否曾成功握手。
func (c *Client) session(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	token, err := tokenOf(ctx, c.cfg.TokenSource, c.cfg.Token)
	if err != nil {
		return false, fmt.Errorf("token: %w", err)
	}
	conn, _, err := websocket.Dial(ctx, c.cfg.URL, nil)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	var wmu sync.Mutex
	send := func(m *v1.NodeMessage) error {
		b, err := proto.Marshal(m)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		return conn.Write(ctx, websocket.MessageBinary, b)
	}

	err = send(&v1.NodeMessage{Msg: &v1.NodeMessage_Hello{Hello: &v1.Hello{
		NodeId: c.cfg.NodeID, Token: token, BusinessLine: c.cfg.BusinessLine, EndUser: c.cfg.EndUser,
		Label: c.cfg.Label, Kind: c.cfg.Kind, HostApp: c.cfg.HostApp, SdkVersion: Version,
		Capabilities: c.cfg.Executor.Specs(),
	}}})
	if err != nil {
		return false, err
	}
	first, err := c.read(ctx, conn)
	if err != nil {
		return false, err
	}
	w := first.GetWelcome()
	if w == nil {
		return false, fmt.Errorf("expected welcome, got %T", first.GetMsg())
	}
	c.cfg.Logger.Info("node connected", "label", w.GetLabel())
	if c.cfg.OnConnected != nil {
		c.cfg.OnConnected(w.GetLabel())
	}

	for {
		m, err := c.read(ctx, conn)
		if err != nil {
			return true, err
		}
		switch m := m.GetMsg().(type) {
		case *v1.GatewayMessage_Invoke:
			go func(inv *v1.Invoke) {
				res, err := c.cfg.Executor.Execute(ctx, inv)
				if err != nil || res == nil {
					if err != nil && !errors.Is(err, context.Canceled) {
						c.cfg.Logger.Error("execute failed", "call", inv.GetCallId(), "err", err)
					}
					return
				}
				_ = send(&v1.NodeMessage{Msg: &v1.NodeMessage_Result{Result: res}})
			}(m.Invoke)
		case *v1.GatewayMessage_Cancel:
			c.cfg.Executor.Cancel(m.Cancel.GetCallId())
		case *v1.GatewayMessage_ResultAck:
			// 结果已持久化；Ledger 中的记录保留用于去重。
		}
	}
}

func (c *Client) read(ctx context.Context, conn *websocket.Conn) (*v1.GatewayMessage, error) {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	m := &v1.GatewayMessage{}
	return m, proto.Unmarshal(b, m)
}

// pruneLoop 每小时清理一次超过保留期的账本记录：调用结果可能含有个人数据，不应在设备上长期留存。
func (c *Client) pruneLoop(ctx context.Context) {
	retention := c.cfg.LedgerRetention
	if retention <= 0 {
		retention = DefaultLedgerRetention
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := c.cfg.Executor.Prune(time.Now().Add(-retention)); err != nil {
			c.cfg.Logger.Warn("ledger prune failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
