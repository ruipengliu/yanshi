package pg

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// TopicDeletions 的负载是 Session ID：它有了删除记录（ADR-0015），订阅它的事件流随之结束。
	TopicDeletions Topic = "yanshi_deletions"
)

// Notifier 用一条专用连接 LISTEN 通知频道，并按 (频道, 键) 把通知分发给订阅者。
//
// 频道按分片命名（Channel，ADR-0028）：主题加上键的哈希分片。连接只 LISTEN 本进程有订阅者的频道，
// 每条通知因此只送到关心它的进程；分片数为 1 时频道名就是主题名。分片数是部署配置，所有进程必须一致：
// 不一致时通知发到对方没有监听的频道，等待者只能靠校准兜底。
//
// 通知只是"可能有变化"的提示，订阅者收到后重新检查状态。等待者不各自轮询（延展性评审 §8.2：空闲订阅的轮询
// 与订阅数成正比）；通知只在以下情形可能丢失，各有对策：
//   - 频道的 LISTEN 尚未生效：生效后唤醒该频道的订阅者重查；等待者在 LISTEN 生效之后才做第一次检查；
//   - 连接断开或半开：心跳（本连接定期 NOTIFY 自己监听的私有频道）在 HeartbeatTimeout 内没有回来即重建连接；
//     重建后缓存作废，全部键按 RecheckRate 分批重查，而不是同时重查；
//   - 其余无法预见的情形：校准按轮转顺序唤醒订阅者重查，每个键至多每 CalibrateInterval 一次，
//     且每秒不超过 CalibrateBudget 个键。
type Notifier struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	shards uint32

	// Heartbeat 是心跳间隔，HeartbeatTimeout 是心跳未回来即判定连接失效的时限。
	Heartbeat, HeartbeatTimeout time.Duration
	// CalibrateInterval 是每个键两次校准之间的目标间隔；CalibrateBudget 是每秒至多校准的键数，键很多时
	// 间隔随之拉长。RecheckRate 是连接重建后每秒重查的键数。
	CalibrateInterval time.Duration
	CalibrateBudget   float64
	RecheckRate       float64

	running atomic.Bool

	mu   sync.Mutex
	subs map[string]map[*subscription]struct{}
	// ring 是有订阅者的键的轮转顺序（校准）；inRing 防止重复加入；credit 是尚未用掉的校准额度。
	ring   []string
	inRing map[string]bool
	cursor int
	credit float64
	// pending 是连接重建后待重查的键。
	pending []string
	// listening 是当前连接已经 LISTEN 的频道。频道在连接存续期间只增不减：取消监听后再监听，
	// 中间的通知会丢失，而缓存（latest）会把丢失前的旧值当作最新值。
	listening map[string]bool
	// added 在有订阅者的频道尚未 LISTEN 时收到信号，使监听循环中断等待、补上 LISTEN。
	added chan struct{}
	// latest 是各键最近通知的值，只记录已 LISTEN 频道上的通知，因此只要记录始于本次连接建立之后，
	// 它就是可信的：缓存中没有更新的值，说明对应的通知尚未到达（到达时会唤醒等待者）。
	// 连接失效时清空（期间的通知可能丢失）；按最近使用淘汰，缺失的键回退到查询。
	latest *lru
	// generation 在每次建立连接并完成 LISTEN 后加一。
	generation uint64
	// pid 是 LISTEN 连接的后端进程号（诊断与测试）。
	pid atomic.Uint32
	// connected 表示 LISTEN 连接已建立、已有订阅的频道都已 LISTEN（就绪检查）。
	connected atomic.Bool
}

// subscription 是一个订阅者。recheck 表示它被校准或重连后的重查唤醒：应以查询为准，而不是缓存的通知值。
type subscription struct {
	key     string
	channel string
	ch      chan struct{}
	recheck bool
}

const latestCap = 100_000

// 默认值见 Notifier 的字段说明。
const (
	DefaultHeartbeat         = 10 * time.Second
	DefaultHeartbeatTimeout  = 10 * time.Second
	DefaultCalibrateInterval = 5 * time.Minute
	DefaultCalibrateBudget   = 50
	DefaultRecheckRate       = 2000
)

// listenWait 是等待者等 LISTEN 生效的上限：监听循环卡住时不至于永远不做第一次检查。
const listenWait = 5 * time.Second

// calibrateTick 是校准与重查的节拍。
const calibrateTick = 100 * time.Millisecond

var errHeartbeat = errors.New("pg notifier: heartbeat not received")

func NewNotifier(pool *pgxpool.Pool, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Notifier{pool: pool, logger: logger, shards: 1, subs: map[string]map[*subscription]struct{}{},
		inRing: map[string]bool{}, listening: map[string]bool{}, added: make(chan struct{}, 1), latest: newLRU(latestCap),
		Heartbeat: DefaultHeartbeat, HeartbeatTimeout: DefaultHeartbeatTimeout, CalibrateInterval: DefaultCalibrateInterval,
		CalibrateBudget: DefaultCalibrateBudget, RecheckRate: DefaultRecheckRate}
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

// Run 维持 LISTEN 连接直到 ctx 结束，断开后重连；同时执行校准与重连后的重查。
func (n *Notifier) Run(ctx context.Context) {
	n.running.Store(true)
	defer n.running.Store(false)
	defer n.connected.Store(false)
	done := make(chan struct{})
	go func() { defer close(done); n.calibrateLoop(ctx) }()
	defer func() { <-done }()
	for ctx.Err() == nil {
		err := n.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		n.connected.Store(false)
		n.logger.Warn("pg notifier reconnecting", "err", err)
		n.invalidate() // 断线期间可能漏掉通知：缓存作废，重连后分批重查
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// Generation 返回连接的代：每次建立连接并完成 LISTEN 后加一。
func (n *Notifier) Generation() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.generation
}

// ListenerPID 返回当前 LISTEN 连接的后端进程号，没有连接时为 0。用于诊断与测试。
func (n *Notifier) ListenerPID() uint32 { return n.pid.Load() }

func (n *Notifier) listen(ctx context.Context) error {
	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	fail := func(err error) error {
		// 连接状态未知，丢弃而不是放回池中。
		_ = conn.Conn().Close(context.Background())
		return err
	}
	n.pid.Store(conn.Conn().PgConn().PID())
	hbChannel := fmt.Sprintf("yanshi_hb_%d_%d", conn.Conn().PgConn().PID(), time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{hbChannel}.Sanitize()); err != nil {
		return fail(err)
	}
	n.mu.Lock()
	clear(n.listening)
	reconnect := n.generation > 0
	n.mu.Unlock()
	first := true
	nextBeat, sentAt := time.Now().Add(n.Heartbeat), time.Time{}
	for {
		for _, ch := range n.unlistened() {
			if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
				return fail(err)
			}
			n.markListening(ch, first && reconnect)
		}
		if first {
			n.onConnected(reconnect)
			first = false
		}
		now := time.Now()
		if !sentAt.IsZero() && now.Sub(sentAt) >= n.HeartbeatTimeout {
			return fail(errHeartbeat)
		}
		if sentAt.IsZero() && !now.Before(nextBeat) {
			// 在本连接上 NOTIFY 自己监听的私有频道：语句往返说明连接仍在（半开的连接在时限内回不来），
			// 收到这条通知说明 LISTEN 仍然生效。不经连接池，池被占满时不会误判。
			hctx, cancel := context.WithTimeout(ctx, n.HeartbeatTimeout)
			_, err := conn.Exec(hctx, "NOTIFY "+pgx.Identifier{hbChannel}.Sanitize())
			cancel()
			if err != nil {
				return fail(fmt.Errorf("%w: %v", errHeartbeat, err))
			}
			sentAt, nextBeat = now, now.Add(n.Heartbeat)
		}
		wake := nextBeat
		if !sentAt.IsZero() {
			wake = sentAt.Add(n.HeartbeatTimeout)
		}
		// 等待通知、新频道要监听或心跳到点。中断等待（取消 ctx）不会关闭连接：pgx 以读超时实现取消，
		// 未读完的消息留在缓冲区中，下次等待继续读取。
		wctx, cancel := context.WithDeadline(ctx, wake)
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
			return fail(err)
		}
		switch {
		case note == nil:
		case note.Channel == hbChannel:
			sentAt = time.Time{}
		default:
			n.signal(note.Channel, note.Payload)
		}
	}
}

// Ready 报告 LISTEN 连接是否已建立：未建立时通知收不到，本进程的等待者只能靠校准，进程不应接收流量。
func (n *Notifier) Ready() bool { return n.connected.Load() }

// onConnected 在连接建立、已有订阅的频道都 LISTEN 之后调用。重连时全部键排队重查。
func (n *Notifier) onConnected(reconnect bool) {
	n.connected.Store(true)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.generation++
	if !reconnect {
		return
	}
	n.pending = n.pending[:0]
	for k := range n.subs {
		n.pending = append(n.pending, k)
	}
}

// unlistened 返回有订阅者而当前连接尚未 LISTEN 的频道。
func (n *Notifier) unlistened() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, set := range n.subs {
		for s := range set {
			if _, ok := n.listening[s.channel]; !ok {
				n.listening[s.channel] = false // 正在 LISTEN
				out = append(out, s.channel)
			}
			break
		}
	}
	return out
}

// markListening 记录频道已 LISTEN，并唤醒其订阅者重查：LISTEN 生效之前发出的通知收不到。
// 重连时不在这里唤醒，由 connected 排队分批重查，避免全部等待者同时查询。
func (n *Notifier) markListening(ch string, reconnecting bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.listening[ch] = true
	if reconnecting {
		return
	}
	for _, set := range n.subs {
		for s := range set {
			if s.channel != ch {
				break
			}
			wake(set, true)
			break
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
	wake(n.subs[k], false)
}

// wake 唤醒订阅者；recheck 为 true 时要求它们以查询为准。
func wake(set map[*subscription]struct{}, recheck bool) {
	for s := range set {
		if recheck {
			s.recheck = true
		}
		select {
		case s.ch <- struct{}{}:
		default:
		}
	}
}

// invalidate 在连接失效时清空缓存：之后的检查一律查询，直到新的通知到达。
func (n *Notifier) invalidate() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.latest.clear()
}

// calibrateLoop 按节拍执行重连后的重查与轮转校准。
func (n *Notifier) calibrateLoop(ctx context.Context) {
	t := time.NewTicker(calibrateTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.calibrate(calibrateTick)
		}
	}
}

// calibrate 唤醒一批订阅者重查：先是重连后待重查的键（每秒 RecheckRate 个），再按轮转顺序校准
// （额度按"每个键每 CalibrateInterval 一次"累积，每秒不超过 CalibrateBudget）。
func (n *Notifier) calibrate(tick time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for range max(int(n.RecheckRate*tick.Seconds()), 1) {
		if len(n.pending) == 0 {
			break
		}
		k := n.pending[len(n.pending)-1]
		n.pending = n.pending[:len(n.pending)-1]
		wake(n.subs[k], true)
	}
	// 移除已没有订阅者的键（惰性维护轮转顺序）。
	live := n.ring[:0]
	for _, k := range n.ring {
		if len(n.subs[k]) > 0 {
			live = append(live, k)
		} else {
			delete(n.inRing, k)
		}
	}
	clear(n.ring[len(live):])
	n.ring = live
	if len(n.ring) == 0 || n.CalibrateInterval <= 0 {
		n.credit = 0
		return
	}
	limit := n.CalibrateBudget * tick.Seconds()
	n.credit = min(n.credit+float64(len(n.ring))*tick.Seconds()/n.CalibrateInterval.Seconds(), max(limit, 1))
	for ; n.credit >= 1; n.credit-- {
		if n.cursor >= len(n.ring) {
			n.cursor = 0
		}
		wake(n.subs[n.ring[n.cursor]], true)
		n.cursor++
	}
}

func (n *Notifier) subscribe(topic Topic, key string) *subscription {
	channel := n.Channel(topic, key)
	s := &subscription{key: channel + "\x00" + key, channel: channel, ch: make(chan struct{}, 1)}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.subs[s.key] == nil {
		n.subs[s.key] = map[*subscription]struct{}{}
	}
	n.subs[s.key][s] = struct{}{}
	if !n.inRing[s.key] {
		n.inRing[s.key] = true
		n.ring = append(n.ring, s.key)
	}
	if _, ok := n.listening[channel]; !ok {
		select {
		case n.added <- struct{}{}:
		default:
		}
	}
	return s
}

func (n *Notifier) unsubscribe(s *subscription) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.subs[s.key], s)
	if len(n.subs[s.key]) == 0 {
		delete(n.subs, s.key)
	}
}

// takeRecheck 返回并清除订阅者的重查标记。
func (n *Notifier) takeRecheck(s *subscription) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	r := s.recheck
	s.recheck = false
	return r
}

// awaitListening 等到订阅的频道 LISTEN 生效（至多 listenWait）：之前的检查可能漏掉检查与 LISTEN 之间的通知，
// 生效时还要再查一次，不如等生效后再查。监听循环没有运行时（单元测试）不等。
func (n *Notifier) awaitListening(ctx context.Context, s *subscription) {
	if !n.running.Load() {
		return
	}
	n.mu.Lock()
	ok := n.listening[s.channel]
	n.mu.Unlock()
	if ok {
		return
	}
	t := time.NewTimer(listenWait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-s.ch:
		// 生效时的唤醒带有重查标记，留给等待循环处理。
	case <-t.C:
	}
}

// Subscribe 订阅 (topic, key) 的通知；返回的通道容量为 1，多次通知会合并。
func (n *Notifier) Subscribe(topic Topic, key string) (<-chan struct{}, func()) {
	s := n.subscribe(topic, key)
	return s.ch, func() { n.unsubscribe(s) }
}

// WaitAbove 阻塞直到 (topic, key) 的值大于 after，返回该值。有缓存的通知值时直接使用；
// 只有缓存缺失（尚无带值的通知、连接重建、缓存淘汰）或被唤醒重查时才调用 query 查询。
func (n *Notifier) WaitAbove(ctx context.Context, topic Topic, key string, after uint64, query func(context.Context) (uint64, error)) (uint64, error) {
	s := n.subscribe(topic, key)
	defer n.unsubscribe(s)
	n.awaitListening(ctx, s)
	for {
		v, ok := uint64(0), false
		if !n.takeRecheck(s) {
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
		case <-s.ch:
		}
	}
}

// Latest 返回本次连接期间收到的 (topic, key) 的最新值；没有时 ok 为 false。
func (n *Notifier) Latest(topic Topic, key string) (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.latest.get(n.Channel(topic, key) + "\x00" + key)
}

// WaitFor 在 check 返回 true 前阻塞：先订阅、等 LISTEN 生效再检查，避免检查与订阅之间的通知丢失；
// 每次通知、重查与校准时重新检查。
func (n *Notifier) WaitFor(ctx context.Context, topic Topic, key string, check func(context.Context) (bool, error)) error {
	s := n.subscribe(topic, key)
	defer n.unsubscribe(s)
	n.awaitListening(ctx, s)
	for {
		n.takeRecheck(s)
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ch:
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
