package pg

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 通知频道。负载为键（Session ID 或 Node ID），或 "键:值"——值是单调递增的数字（如日志末尾 seq），
// 等待者可以直接用它判断条件，而不必再查询（WaitAbove）。
const (
	ChannelEvents = "yanshi_events"
	ChannelInbox  = "yanshi_inbox"
	// ChannelWork 的负载是队列表名：有新工作入队（workqueue.Signaler）。
	ChannelWork = "yanshi_work"
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
	// latest 是各键最近通知的值。LISTEN 连接收到全部通知，因此只要记录始于本次连接建立之后，
	// 它就是可信的：缓存中没有更新的值，说明对应的通知尚未到达（到达时会唤醒等待者）。
	// 连接重建时清空（期间的通知可能丢失）；超过 latestCap 时也整体清空，缺失的键回退到查询。
	latest map[string]uint64
}

const latestCap = 100_000

// PollInterval 是等待者在没有通知时重新检查的间隔。
const PollInterval = 2 * time.Second

func NewNotifier(pool *pgxpool.Pool, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Notifier{pool: pool, logger: logger, subs: map[string]map[chan struct{}]struct{}{}, latest: map[string]uint64{}}
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
	for _, ch := range []string{ChannelEvents, ChannelInbox, ChannelWork} {
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
		n.signal(note.Channel, note.Payload)
	}
}

func (n *Notifier) signal(channel, payload string) {
	key, value := payload, uint64(0)
	if i := strings.LastIndexByte(payload, ':'); i >= 0 {
		if v, err := strconv.ParseUint(payload[i+1:], 10, 64); err == nil {
			key, value = payload[:i], v
		}
	}
	k := channel + "\x00" + key
	n.mu.Lock()
	defer n.mu.Unlock()
	if value > n.latest[k] {
		if len(n.latest) >= latestCap {
			clear(n.latest)
		}
		n.latest[k] = value
	}
	for ch := range n.subs[k] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	clear(n.latest)
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

// WaitAbove 阻塞直到 (channel, key) 的值大于 after，返回该值。有缓存的通知值时直接使用；
// 只有缓存缺失（尚无带值的通知、连接重建、缓存清空）或轮询兜底时才调用 query 查询。
func (n *Notifier) WaitAbove(ctx context.Context, channel, key string, after uint64, query func(context.Context) (uint64, error)) (uint64, error) {
	ch, unsubscribe := n.Subscribe(channel, key)
	defer unsubscribe()
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	polled := false
	for {
		v, ok := uint64(0), false
		if !polled {
			v, ok = n.value(channel, key)
		}
		if !ok {
			var err error
			if v, err = query(ctx); err != nil {
				return 0, err
			}
		}
		if v > after {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ch:
			polled = false
		case <-t.C:
			polled = true // 兜底：通知可能丢失，以查询为准
		}
	}
}

// Latest 返回本次连接期间收到的 (channel, key) 的最新值；没有时 ok 为 false。
func (n *Notifier) Latest(channel, key string) (uint64, bool) { return n.value(channel, key) }

func (n *Notifier) value(channel, key string) (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.latest[channel+"\x00"+key]
	return v, ok
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
