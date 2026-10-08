package pg

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 通知频道。负载为 Session ID 或 Node ID。
const (
	ChannelEvents = "yanshi_events"
	ChannelInbox  = "yanshi_inbox"
)

// Notifier 用一条专用连接 LISTEN 全部频道，并按 (频道, 键) 把通知分发给订阅者。
//
// 通知只是"可能有变化"的提示：订阅者收到后必须重新读取状态，并以 PollInterval 轮询兜底，
// 因为连接重建期间的通知会丢失。
type Notifier struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// PollInterval 是等待者在没有通知时重新检查的间隔。
const PollInterval = 2 * time.Second

func NewNotifier(pool *pgxpool.Pool, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Notifier{pool: pool, logger: logger, subs: map[string]map[chan struct{}]struct{}{}}
}

// Run 维持 LISTEN 连接直到 ctx 结束，断开后重连。
func (n *Notifier) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := n.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		n.logger.Warn("pg notifier reconnecting", "err", err)
		n.broadcast() // 断线期间可能漏掉通知，让所有等待者重新检查
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (n *Notifier) listen(ctx context.Context) error {
	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	for _, ch := range []string{ChannelEvents, ChannelInbox} {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return err
		}
	}
	for {
		note, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			// 连接状态未知，丢弃而不是放回池中。
			_ = conn.Conn().Close(context.Background())
			return err
		}
		n.signal(note.Channel + "\x00" + note.Payload)
	}
}

func (n *Notifier) signal(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs[key] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, set := range n.subs {
		for ch := range set {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Subscribe 订阅 (channel, key) 的通知；返回的通道容量为 1，多次通知会合并。
func (n *Notifier) Subscribe(channel, key string) (<-chan struct{}, func()) {
	k := channel + "\x00" + key
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	if n.subs[k] == nil {
		n.subs[k] = map[chan struct{}]struct{}{}
	}
	n.subs[k][ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.subs[k], ch)
		if len(n.subs[k]) == 0 {
			delete(n.subs, k)
		}
	}
}

// WaitFor 在 check 返回 true 前阻塞：先订阅再检查，避免检查与订阅之间的通知丢失。
func (n *Notifier) WaitFor(ctx context.Context, channel, key string, check func(context.Context) (bool, error)) error {
	ch, unsubscribe := n.Subscribe(channel, key)
	defer unsubscribe()
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-t.C:
		}
	}
}
