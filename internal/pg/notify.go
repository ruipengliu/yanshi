package pg

import (
	"container/list"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Topic 是通知的主题。通知的负载为键（Session ID 或 Node ID），或 "键:值"——值是单调递增的数字
// （如日志末尾 seq），等待者可以直接用它判断条件，而不必再查询（WaitAbove）。
type Topic string

const (
	TopicEvents Topic = "yanshi_events"
	TopicInbox  Topic = "yanshi_inbox"
	// TopicWork 的负载是队列表名：有新工作入队（workqueue.Signaler）。
	TopicWork Topic = "yanshi_work"
	// TopicPresence 的负载是 Session ID：其在场内容有变化。
	TopicPresence Topic = "yanshi_presence"
)

// Notifier 用一条专用连接 LISTEN 通知频道，并按 (频道, 键) 把通知分发给订阅者。
//
// 频道按分片命名（Channel，ADR-0028）：主题加上键的哈希分片。连接只 LISTEN 本进程有订阅者的频道，
// 每条通知因此只送到关心它的进程；分片数为 1 时频道名就是主题名。分片数是部署配置，所有进程必须一致：
// 不一致时通知发到对方没有监听的频道，等待者只能靠轮询兜底。
//
// 通知只是"可能有变化"的提示：订阅者收到后必须重新读取状态，并以 PollInterval 轮询兜底，
// 因为连接重建期间的通知会丢失。
type Notifier struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	shards uint32

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
	// listening 是当前连接已经 LISTEN 的频道。频道在连接存续期间只增不减：取消监听后再监听，
	// 中间的通知会丢失，而缓存（latest）会把丢失前的旧值当作最新值。
	listening map[string]bool
	// added 在有订阅者的频道尚未 LISTEN 时收到信号，使监听循环中断等待、补上 LISTEN。
	added chan struct{}
	// latest 是各键最近通知的值，只记录已 LISTEN 频道上的通知，因此只要记录始于本次连接建立之后，
	// 它就是可信的：缓存中没有更新的值，说明对应的通知尚未到达（到达时会唤醒等待者）。
	// 连接重建时清空（期间的通知可能丢失）；按最近使用淘汰，缺失的键回退到查询。
	latest *lru
}

const latestCap = 100_000

// PollInterval 是等待者在没有通知时重新检查的间隔。
const PollInterval = 2 * time.Second

func NewNotifier(pool *pgxpool.Pool, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Notifier{pool: pool, logger: logger, shards: 1, subs: map[string]map[chan struct{}]struct{}{},
		listening: map[string]bool{}, added: make(chan struct{}, 1), latest: newLRU(latestCap)}
}

// WithShards 设置通知分片数（默认 1），须在 Run 与任何订阅之前调用。
func (n *Notifier) WithShards(shards int) *Notifier {
	n.shards = uint32(max(shards, 1))
	return n
}

// Channel 返回 (topic, key) 的通知所在频道。nil 的 Notifier（未启用通知）使用单一分片。
func (n *Notifier) Channel(topic Topic, key string) string {
	if n == nil || n.shards <= 1 {
		return string(topic)
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return fmt.Sprintf("%s_%d", topic, h.Sum32()%n.shards)
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
	n.mu.Lock()
	clear(n.listening)
	n.mu.Unlock()
	for {
		for _, ch := range n.unlistened() {
			if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
				_ = conn.Conn().Close(context.Background())
				return err
			}
			n.markListening(ch)
		}
		// 等待通知，或有新频道要监听。中断等待（取消 ctx）不会关闭连接：pgx 以读超时实现取消，
		// 未读完的消息留在缓冲区中，下次等待继续读取。
		wctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-n.added:
				cancel()
			case <-wctx.Done():
			}
		}()
		note, err := conn.Conn().WaitForNotification(wctx)
		interrupted := wctx.Err() != nil && ctx.Err() == nil
		cancel()
		if err != nil && !interrupted {
			// 连接状态未知，丢弃而不是放回池中。
			_ = conn.Conn().Close(context.Background())
			return err
		}
		if note != nil {
			n.signal(note.Channel, note.Payload)
		}
	}
}

// unlistened 返回有订阅者而当前连接尚未 LISTEN 的频道。
func (n *Notifier) unlistened() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for k := range n.subs {
		ch, _, _ := strings.Cut(k, "\x00")
		if _, ok := n.listening[ch]; !ok {
			n.listening[ch] = false // 正在 LISTEN
			out = append(out, ch)
		}
	}
	return out
}

// markListening 记录频道已 LISTEN，并唤醒其订阅者：LISTEN 生效之前发出的通知收不到，订阅者须重新检查。
func (n *Notifier) markListening(ch string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.listening[ch] = true
	for k, set := range n.subs {
		if strings.HasPrefix(k, ch+"\x00") {
			wake(set)
		}
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
	if v, _ := n.latest.get(k); value > v {
		n.latest.put(k, value)
	}
	wake(n.subs[k])
}

func wake(set map[chan struct{}]struct{}) {
	for ch := range set {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.latest.clear()
	for _, set := range n.subs {
		wake(set)
	}
}

// Subscribe 订阅 (topic, key) 的通知；返回的通道容量为 1，多次通知会合并。
func (n *Notifier) Subscribe(topic Topic, key string) (<-chan struct{}, func()) {
	channel := n.Channel(topic, key)
	k := channel + "\x00" + key
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	if n.subs[k] == nil {
		n.subs[k] = map[chan struct{}]struct{}{}
	}
	n.subs[k][ch] = struct{}{}
	if _, ok := n.listening[channel]; !ok {
		select {
		case n.added <- struct{}{}:
		default:
		}
	}
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

// WaitAbove 阻塞直到 (topic, key) 的值大于 after，返回该值。有缓存的通知值时直接使用；
// 只有缓存缺失（尚无带值的通知、连接重建、缓存淘汰）或轮询兜底时才调用 query 查询。
func (n *Notifier) WaitAbove(ctx context.Context, topic Topic, key string, after uint64, query func(context.Context) (uint64, error)) (uint64, error) {
	ch, unsubscribe := n.Subscribe(topic, key)
	defer unsubscribe()
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	polled := false
	for {
		v, ok := uint64(0), false
		if !polled {
			v, ok = n.Latest(topic, key)
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

// Latest 返回本次连接期间收到的 (topic, key) 的最新值；没有时 ok 为 false。
func (n *Notifier) Latest(topic Topic, key string) (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.latest.get(n.Channel(topic, key) + "\x00" + key)
}

// WaitFor 在 check 返回 true 前阻塞：先订阅再检查，避免检查与订阅之间的通知丢失。
func (n *Notifier) WaitFor(ctx context.Context, topic Topic, key string, check func(context.Context) (bool, error)) error {
	return n.WaitForEvery(ctx, topic, key, PollInterval, check)
}

// WaitForEvery 与 WaitFor 相同，但兜底轮询的间隔为 every：等待者很多、对延迟不敏感时用更长的间隔，
// 以免空闲时的轮询压在数据库上。
func (n *Notifier) WaitForEvery(ctx context.Context, topic Topic, key string, every time.Duration, check func(context.Context) (bool, error)) error {
	ch, unsubscribe := n.Subscribe(topic, key)
	defer unsubscribe()
	t := time.NewTicker(every)
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

// lru 是按最近使用淘汰的缓存。容量满时整体清空会让所有等待者同时回退到查询（活跃键多于容量时反复发生），
// 按最近使用淘汰只影响最久不用的键。
type lru struct {
	cap   int
	order *list.List // 前端最近使用
	items map[string]*list.Element
}

type lruEntry struct {
	key   string
	value uint64
}

func newLRU(capacity int) *lru {
	return &lru{cap: capacity, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(key string) (uint64, bool) {
	e, ok := c.items[key]
	if !ok {
		return 0, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*lruEntry).value, true
}

func (c *lru) put(key string, value uint64) {
	if e, ok := c.items[key]; ok {
		e.Value.(*lruEntry).value = value
		c.order.MoveToFront(e)
		return
	}
	c.items[key] = c.order.PushFront(&lruEntry{key, value})
	if c.order.Len() > c.cap {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.items, old.Value.(*lruEntry).key)
	}
}

func (c *lru) clear() {
	c.order.Init()
	clear(c.items)
}
