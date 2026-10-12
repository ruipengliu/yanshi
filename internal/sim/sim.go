// Package sim 是确定性模拟测试框架（docs/design/m0-core-primitives.md §6）。
//
// 一个 World 内的全部组件共享一个种子化随机源与虚拟时钟，并在单个 goroutine 中
// 按随机交错推进 Worker 与客户端，同时注入故障。同一种子必然产生同一条执行轨迹。
package sim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/askuser"
	"yanshi/internal/capability"
	"yanshi/internal/card"
	"yanshi/internal/clock"
	"yanshi/internal/delegation"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
	"yanshi/internal/notify"
	"yanshi/internal/presence"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
	"yanshi/sdk/nodesdk"
)

// Stores 是模拟使用的存储实现（app.Stores）；默认为内存实现。
type Stores = app.Stores

type Options struct {
	// NewStores 为 nil 时使用内存实现；传入的时钟是模拟的虚拟时钟。
	NewStores func(c clock.Clock) Stores
	Seed      uint64
	Workers   int
	Sessions  int
	// Ticks 是注入故障与客户端动作的阶段长度。
	Ticks int
	// Faults 为 false 时不注入任何故障。
	Faults bool
	// LongRuns 让脚本化模型在一个 Run 内持续调用工具上百轮，并降低中断频率，
	// 用于检验小时级 Run 的上下文压缩与恢复（docs/design/m2-long-runs.md §8）。
	LongRuns bool
}

// Stats 统计模拟中被覆盖到的路径，防止模拟"空转"。
type Stats struct {
	Runs, Steers, Interrupts, Crashes, ModelErrors int
	Takeovers, OutcomeUnknown, Retried, Failed     int
	NodeCrashes, Redeliveries, Approvals, Denials  int
	ControllerCrashes, SandboxExecs, Reaps         int
	Suspensions, DeviceResults, Timeouts           int
	// TurnLimited 是因超过 AgentDef.MaxTurns 而失败的 Run，属于策略结果而非故障。
	TurnLimited int
	// Session 生命周期：关闭、删除、Janitor 清理中途崩溃、完成的删除。
	Closes, Deletions, JanitorCrashes, DeletionsCompleted int
	// Memory：带有结果的召回次数、成功写入次数。
	Recalls, MemorySaves int
	// 上下文压缩：压缩次数、摘要调用中的故障、模型报告的上下文超长、
	// 压缩之后同一 Run 开启的 Attempt（接管或恢复），以及完成的长 Run（≥ longRunTurns 轮）。
	Compactions, SummaryFaults, Overflows      int
	AttemptsAfterCompaction, LongRunsCompleted int
	// EnqueueInterleavings 是在客户端写入日志并入队之后、返回之前插入 Worker 步骤的次数。
	EnqueueInterleavings int
	// 外部写入（输入、审批、回答、Node 结果）的存储故障：WakeFaults 是注入的次数（含已生效但报告失败的），
	// UncertainRetries 是客户端随后以同一输入 ID 重试的次数。
	WakeFaults, UncertainRetries int
	// CatalogFaults 是 Worker 查询设备目录（解析工具）时注入的暂时性故障。
	CatalogFaults int
	// ResumedClaims 是以恢复名额认领的长任务（用户刚做出审批或回答，ADR-0029）。
	ResumedClaims int
	// 委托（docs/design/m5-multi-agent.md）：创建的子 Session，子 Run 写回父调用的结果（完成、失败或中断）。
	// ChildDeletionRaces 是重新派发时、在检查删除记录与创建子 Session 之间删除并清理完子 Session 的次数（评审 §11 问题 2）。
	// CascadeFaults 是中断或关闭父 Session 后撤回委托失败的次数（评审 §11 问题 10）：客户端有时重试、有时放弃，
	// 放弃时子 Run 由子 Worker 在下一次调用模型前停下（Delegator.Reconcile；模拟中的 Run 很短，这条路径由
	// e2e 的 TestDelegationOrphanStops 覆盖）。
	Delegations, DelegationResults, ChildDeletionRaces, CascadeFaults int
	// 判断（docs/design/m6-decisions.md）：记录的判断，其中判断不可用、走默认路径的。
	Decisions, DecisionFallbacks int
	// PastRuns 是移出投影、只剩摘要的终态 Run（有界投影，延展性评审 §8.3）。
	PastRuns int
	// 配额：因配额挂起的次数、提交时被拒绝的次数、挂起后恢复并结束的 Run；注销账号的次数。
	QuotaSuspensions, QuotaRejections, QuotaResumes, AccountDeletions int
	// 灰度：撤回版本后在下一个 Run 切换到稳定版本的次数。
	AgentSwitches int
	// 内容安全：被拒绝的输入、被拦截的输出、提供商错误。
	InputBlocks, OutputBlocks, ModerationErrors int
	// Memory 写入闸门要求的审批（ADR-0023）。
	MemoryApprovals int
	// 在场：写入次数，以及写入时 Session 已被删除、因"先写、后查"而撤回的次数。
	PresenceWrites, PresenceWithdrawn int
	// ask_user（ADR-0025）：提问、点选回答、打字回答、被拒绝的不合法回答、无人回答而超时。
	Questions, Answers, TypedAnswers, InvalidAnswers, QuestionTimeouts int
	// 附带界面上下文的输入；以同一输入 ID 重试而被去重的提交。
	UIContextInputs, DuplicateInputs int
	// 卡片（ADR-0032）：展示的卡片，提交的卡片动作，其中因同一版卡片上的同一动作已生效而被去重的，以及 Run 等回答时
	// 作为提问的回答的。
	Cards, CardActions, CardDuplicates, CardAnswers int
	// 提醒：推送出去的、因用户正在看而不推的、推送通道失败（含令牌无效）的次数。
	Notifications, NotificationsWatching, PushFailures int
	// Call：开始（其中取代旧 Call 的）、写入的转写、被拦截的转写、派生的任务，以及中转进程的写入因 Call
	// 已结束、Session 已关闭或删除而被拒绝的次数。
	// CallAnswers 是派生的任务恰好回答了 Run 正在等的提问的次数。
	CallsStarted, CallsReplaced, CallTranscripts, CallTranscriptsBlocked, CallTasks, CallAnswers, CallConflicts int
	// Worker 的日志追加失败（含已生效但报告失败的）。
	StoreFaults int
	// 工作类别（ADR-0029）：Worker 优雅停机，以及移交（以 handoff 挂起：交互 Worker 移交成为长任务的 Run，
	// 或优雅停机时移交持有的 Run）。
	GracefulStops, Handoffs int
	// 后台调用（ADR-0035）：转入后台、设备结果送达、到期、用户取消的次数，以及设备下线时唤醒等它的 Session 的次数。
	Backgrounded, BackgroundResults, BackgroundTimeouts, BackgroundCancels, DisconnectWakes int
}

// storeFault 是注入的存储故障（数据库超时、连接中断）：written 表示追加其实已经生效，调用方却只看到错误。
type storeFault struct{ written bool }

func (f *storeFault) Error() string {
	if f.written {
		return "simulated storage failure (append applied)"
	}
	return "simulated storage failure"
}

// flakyLog 是 Worker 看到的日志：追加偶尔失败，一半在写入之前，一半在写入之后（结果未知）。
// Worker 必须能从两者恢复：重试写入，而不是重新执行已经产生副作用的调用。
type flakyLog struct {
	eventlog.Log
	w *World
}

func (l flakyLog) Append(ctx context.Context, sid string, expected uint64, events ...*v1.Event) (uint64, error) {
	if !l.w.faults || !l.w.chance(0.03) {
		return l.Log.Append(ctx, sid, expected, events...)
	}
	l.w.Stats.StoreFaults++
	if l.w.chance(0.5) {
		return 0, &storeFault{}
	}
	if _, err := l.Log.Append(ctx, sid, expected, events...); err != nil {
		return 0, err
	}
	return 0, &storeFault{written: true}
}

// flakyWaking 向外部写入（输入、审批、回答、Node 结果）注入存储故障：没有生效，或已经生效、调用方却只看到错误。
// 写入与入队是原子的（CommitAndWake），所以无论哪种，Run 都无须新的输入就能推进，quiesce 检查这一点。
// 写入生效后调用 afterEnqueue（如有），在客户端写入与返回之间插入 Worker 的一步。
type flakyWaking struct {
	eventlog.WakingLog
	w *World
}

func (l flakyWaking) AppendAndEnqueue(ctx context.Context, sid string, expected uint64, class workqueue.Class, events ...*v1.Event) (uint64, error) {
	if l.w.faults && l.w.chance(0.04) {
		l.w.Stats.WakeFaults++
		if l.w.chance(0.5) {
			return 0, &storeFault{}
		}
		if _, err := l.WakingLog.AppendAndEnqueue(ctx, sid, expected, class, events...); err != nil {
			return 0, err
		}
		if err := l.w.interleave(); err != nil {
			return 0, err
		}
		return 0, &storeFault{written: true}
	}
	head, err := l.WakingLog.AppendAndEnqueue(ctx, sid, expected, class, events...)
	if err != nil {
		return 0, err
	}
	return head, l.w.interleave()
}

// errDirectoryDown 是注入的设备目录故障：暂时性的，不能使 Run 立即失败（延展性评审 §8.1）。
var errDirectoryDown = errors.New("sim: node directory unavailable")

// flakyDirectory 让 Worker 解析工具时查询设备目录偶尔失败。
type flakyDirectory struct {
	node.Directory
	w *World
}

func (d flakyDirectory) List(ctx context.Context, scope node.Scope) ([]*node.Info, error) {
	if d.w.faults && d.w.chance(0.05) {
		d.w.Stats.CatalogFaults++
		return nil, errDirectoryDown
	}
	return d.Directory.List(ctx, scope)
}

// interleave 不可重入：插入的 Worker 步骤本身可能写入（如委托时向子 Session 提交任务），不能在其中再插入一步。
func (w *World) interleave() error {
	hook := w.afterEnqueue
	if hook == nil {
		return nil
	}
	w.afterEnqueue = nil
	defer func() { w.afterEnqueue = hook }()
	return hook()
}

// poolQueue 是一个 Worker 看到的工作队列：检查认领到的工作属于该 Worker 的池（ADR-0029）。
type poolQueue struct {
	workqueue.Queue
	w *World
}

func (q poolQueue) Claim(ctx context.Context, holder string, ttl time.Duration, pool workqueue.Pool) (*workqueue.Lease, error) {
	l, err := q.Queue.Claim(ctx, holder, ttl, pool)
	if err == nil && !pool.Admits(l.Class) {
		q.w.poolViolation = fmt.Errorf("invariant: %s claimed %s of class %+v outside its pool %+v", holder, l.SessionID, l.Class, pool)
	}
	if err == nil && l.Class.Priority == workqueue.Resumed {
		q.w.Stats.ResumedClaims++
	}
	return l, err
}

// interactiveWorker 是只认领交互工作的 Worker 的 ID 前缀：模拟中 0 号 Worker 是交互 Worker。
const interactiveWorker = "wi"

// simContext 是模拟 AgentDef 的上下文配置：窗口很小（工具声明约占 1150，其中 ask_user 约 450、缩短的 show_card 约 30），使压缩频繁发生；须容得下工具声明、摘要、KeepRecent 与一条最大的消息（含界面上下文）。
var simContext = agentdef.Context{Window: 2800, CompactAt: 0.75, KeepRecent: 300, MaxToolResult: 250}

const longRunTurns = 20

const (
	leaseTTL        = 10 * time.Second
	approvalTimeout = 30 * time.Second
	deviceTimeout   = 60 * time.Second
	askTimeout      = 90 * time.Second
)

// simNode 是一台模拟设备：SDK 的 Executor 与 Ledger 跨崩溃保留，连接随上下线变化。
type simNode struct {
	id     string
	hello  *v1.Hello
	exec   *nodesdk.Executor
	ledger *nodesdk.MemLedger
	conn   *node.Conn
	// awayUntil 是下线的设备再次上线的最早时刻：手机长时间离线（ADR-0035 的后台调用）。
	awayUntil time.Time
}

type World struct {
	opts    Options
	stores  Stores
	rng     *rand.Rand
	clock   *clock.Fake
	log     eventlog.Log
	store   *session.Store
	queue   workqueue.Queue
	core    *app.Core
	svc     *service.Service
	agents  *agentdef.Registry
	catalog *capability.Catalog
	hub     *node.Hub
	nodes   []*simNode

	sandboxQueue workqueue.Queue
	provider     *sandbox.Fake
	artifacts    *artifact.Service
	ledger       nodesdk.Ledger
	activity     sandbox.Activity
	controllers  []*sandbox.Controller
	nextC        int
	index        lifecycle.Index
	deletions    lifecycle.Deletions
	janitorQueue workqueue.Queue
	memories     *memory.Service
	presence     *presence.Service
	push         notify.Registry
	notifier     *notify.Notifier
	// notified 记录已提醒过的事（Session/种类/Run/调用），检查至多提醒一次。
	notified map[string]bool
	// cardInputs 记录生效的卡片动作：Session → 推导出的输入 ID → 动作。检查它们在日志中都能被客户端看到（CardActionOf）。
	cardInputs map[string]map[string]*v1.CardAction
	usage      usage.Store
	quotas     *usage.Quotas
	meter      *usage.Meter
	// users 是每个 Session 位置当前的 EndUser；注销账号后换成新的 EndUser。
	users        []string
	deletedUsers []string
	generation   int
	janitors     []*janitor.Janitor
	nextJ        int
	// deleted 与 closed 是已删除、已关闭（已从 sessions 中替换掉）的 Session。
	deleted []string
	closed  []string
	workers []*runtime.Worker
	nextW   int

	sessions []string
	// children 是发现的委托子 Session（discoverChildren），parentOf 是它们的父 Session。
	children  []string
	parentOf  map[string]string
	delegator *delegation.Delegator
	dispatch  *delegation.Router
	// nextInput 生成客户端输入 ID。
	nextInput int
	// calls 是各 Session 上中转进程以为进行中的 Call（可能已过时）；nextCall 生成 Call ID。
	calls    map[string]string
	nextCall int
	// effects 记录非幂等 Capability（进程内与设备上）每个调用 ID 的实际执行次数。
	effects map[string]int
	// withdrawn 是已撤回投递的后台调用（用户取消、Session 关闭）：设备之后不得再执行它们（ADR-0035）。
	withdrawn map[string]bool
	// crash 在一次 Step 或设备执行期间可用：调用它模拟该进程在此刻崩溃。
	crash func()
	// currentCall 是设备正在执行的调用 ID。
	currentCall string
	faults      bool
	// afterEnqueue 非空时，在客户端动作内部的每次入队之后调用。
	afterEnqueue func() error
	// violations 记录脚本化模型观察到的请求级不变量违反（超出窗口、调用配对残缺）。
	violations []string
	// poolViolation 记录 Worker 认领到不属于其池的工作（ADR-0029）。
	poolViolation error
	Stats         Stats
	// Trace 记录每一步的动作，失败时用于定位。
	Trace []string
}

func New(opts Options) (*World, error) {
	w := &World{
		opts:      opts,
		rng:       rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15)),
		clock:     clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		effects:   map[string]int{},
		withdrawn: map[string]bool{},
		faults:    opts.Faults,
	}
	stores := app.Memory(w.clock)
	if opts.NewStores != nil {
		stores = opts.NewStores(w.clock)
	}
	w.stores = stores
	w.log, w.queue = stores.Log, stores.Queue
	maxTurns := 6
	if opts.LongRuns {
		maxTurns = 120
	}
	// 两个版本，配合随机的灰度与撤回（docs/design/m4-agent-rollout.md §7）。
	var defs []*agentdef.Def
	for _, v := range []string{"1", "2"} {
		defs = append(defs, &agentdef.Def{
			Name: "sim", Version: v, Model: "sim/m",
			Capabilities: []string{"echo", "send", "ask_user", "show_card", "memory_save", "memory_forget", "memory_search", "device:*", "sandbox:*", "agent:sim"},
			MaxTurns:     maxTurns, Context: simContext, Memory: agentdef.MemoryConfig{Recall: 3},
		})
	}
	agents, err := agentdef.NewRegistry(defs...)
	if err != nil {
		return nil, err
	}
	w.agents = agents
	if err := w.setRelease(0); err != nil {
		return nil, err
	}
	w.index, w.deletions, w.janitorQueue = stores.Index, stores.Deletions, stores.JanitorQueue
	w.sandboxQueue, w.ledger, w.activity = stores.SandboxQueue, stores.Ledger, stores.Activity
	w.presence = &presence.Service{Store: stores.Presence, Deletions: stores.Deletions, Clock: w.clock}
	w.push, w.notified, w.cardInputs = stores.Push, map[string]bool{}, map[string]map[string]*v1.CardAction{}
	w.calls = map[string]string{}
	w.parentOf = map[string]string{}
	// 配额读缓存的已用金额（usage.SpentCache），计量经同一个缓存写入：单进程中缓存值与实际一致，
	// checkedUsage 的不变量照常成立。
	spent := usage.NewSpentCache(stores.Usage, 30*time.Second, w.clock)
	w.usage = &checkedUsage{Store: spent, truth: stores.Usage, w: w}
	w.quotas = &usage.Quotas{Store: spent, Limits: simLimits(opts.LongRuns)}
	w.meter = &usage.Meter{Store: w.usage, Prices: simPrices, Deletions: w.deletions}
	w.provider = sandbox.NewFake()
	w.provider.ExecFunc = w.sandboxExec
	gw := model.NewGateway()
	gw.Register("sim", &simModel{w: w})
	// 对象图与 serve 相同（app.NewCore）；存储故障在 Stores 的 seam 上注入，其余替身在构造之后替换对应字段。
	// 外部写入（输入、审批、回答、Node 结果）经 flakyWaking；Worker 的追加另经 flakyLog（newWorker）。
	wrapped := stores
	wrapped.Waking = flakyWaking{WakingLog: stores.Waking, w: w}
	w.core = app.NewCore(wrapped, app.Options{
		Clock: w.clock, IDs: ids.Sequential, Agents: agents, Model: gw, Blobs: artifact.NewMemBlobs(), NodeAuth: node.InsecureDevAuth{},
		Local:        []capability.Capability{w.echoCap(), w.sendCap(), simCardCap()},
		SandboxTools: true, DefaultTimeout: deviceTimeout, NodeCacheTTL: 5 * time.Second,
		BackgroundGrace: bgGrace, BackgroundTimeout: bgTimeout,
		Moderator: simModerator{w}, InjectionCheck: simDecider{w}, Meter: w.meter, Quotas: w.quotas, Pusher: simPusher{w},
		LeaseTTL: leaseTTL,
	})
	w.store, w.hub, w.catalog, w.memories, w.artifacts = w.core.Store, w.core.Hub, w.core.Catalog, w.core.Memory, w.core.Artifacts
	w.delegator, w.dispatch, w.svc, w.notifier = w.core.Delegator, w.core.Dispatch, w.core.Service, w.core.Notifier
	// 快照每 5 条写一次，使加载几乎总是走"快照 + 剩余日志"的路径。
	w.store.SnapshotEvery = 5
	w.memories.MaxPerUser = 20
	// Worker 解析工具时查询设备目录偶尔失败；Hub 的登记与上下线不受影响。
	w.catalog.Nodes = flakyDirectory{Directory: stores.Dir, w: w}
	w.catalog.AskTimeout = askTimeout
	w.delegator.BeforeCreate = w.deleteChildMidDispatch
	w.svc.Nodes = flakyCancel{Router: w.dispatch, w: w}
	w.hub.Queue = wakeCounter{Queue: stores.Queue, w: w}
	for range 2 {
		w.controllers = append(w.controllers, w.newController())
	}
	for range 2 {
		w.janitors = append(w.janitors, w.newJanitor())
	}
	for i := range opts.Workers {
		w.workers = append(w.workers, w.newWorker(i))
	}
	for i := range opts.Sessions {
		w.users = append(w.users, fmt.Sprintf("u%d", i))
		id, err := w.svc.Create(context.Background(), service.CreateRequest{
			BusinessLine: "bl", EndUser: w.users[i], Agent: "sim",
		})
		if err != nil {
			return nil, err
		}
		w.sessions = append(w.sessions, id)
		n := w.newNode(i)
		w.nodes = append(w.nodes, n)
		if err := w.connect(n); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *World) newController() *sandbox.Controller {
	w.nextC++
	c := w.core.ControllerConfig(fmt.Sprintf("c%d", w.nextC), w.provider, nil)
	c.Heartbeat, c.IdleTTL = 0, 2*time.Minute // 续约按真实时间，模拟中关闭
	return c
}

func (w *World) stepController(i int) error {
	ctx, cancel := context.WithCancel(context.Background())
	crashed := false
	w.crash = func() { crashed = true; cancel() }
	defer func() { w.crash = nil; cancel() }()
	c := w.controllers[i]
	did, err := c.Step(ctx)
	if crashed {
		w.tracef("crash %s (mid-exec)", c.ID)
		w.Stats.ControllerCrashes++
		w.controllers[i] = w.newController()
		return nil
	}
	if did {
		w.tracef("step %s", c.ID)
	}
	if err != nil {
		return fmt.Errorf("controller %s step: %w", c.ID, err)
	}
	return nil
}

// sandboxExec 是假沙箱的执行：记录副作用，并可能在产生副作用后、返回结果前让控制器崩溃。
func (w *World) sandboxExec(ctx context.Context, id string, _ sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	w.effects[id+"/"+sandbox.CallID(ctx)]++
	w.Stats.SandboxExecs++
	w.clock.Advance(time.Second) // 执行占用时间，按时长计量
	if w.faults && w.chance(0.15) {
		w.crash()
		return nil, ctx.Err()
	}
	return &sandbox.ExecResult{Stdout: []byte("42\n")}, nil
}

func (w *World) newNode(i int) *simNode {
	n := &simNode{id: fmt.Sprintf("node%d", i)}
	n.hello = &v1.Hello{NodeId: n.id, BusinessLine: "bl", EndUser: fmt.Sprintf("u%d", i), Label: "pc", Kind: "desktop",
		PushPlatform: "sim", PushToken: "tok-" + n.id}
	effect := func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
		w.effects[n.id+"/"+w.currentCall]++
		w.checkNotWithdrawn()
		if w.faults && w.chance(0.2) {
			w.crash() // App 在产生副作用后、记录结果前被杀
			return nil, ctx.Err()
		}
		return model.TextBlocks("done"), nil
	}
	read := func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
		w.checkNotWithdrawn()
		if w.faults && w.chance(0.1) {
			w.crash()
			return nil, ctx.Err()
		}
		return model.TextBlocks("file content"), nil
	}
	n.ledger = nodesdk.NewMemLedger()
	n.exec = nodesdk.NewExecutor(n.ledger,
		nodesdk.Capability{Spec: &v1.CapabilitySpec{Name: "dev_read", Idempotent: true, Risk: v1.Risk_RISK_LOW}, Handler: read},
		nodesdk.Capability{Spec: &v1.CapabilitySpec{Name: "dev_send", Risk: v1.Risk_RISK_HIGH}, Handler: effect},
	)
	n.hello.Capabilities = []*v1.CapabilitySpec{
		{Name: "dev_read", Idempotent: true, Risk: v1.Risk_RISK_LOW},
		{Name: "dev_send", Risk: v1.Risk_RISK_HIGH},
	}
	return n
}

func (w *World) connect(n *simNode) error {
	c, err := w.hub.Connect(context.Background(), n.hello)
	if err != nil {
		return err
	}
	n.conn = c
	w.tracef("node %s online", n.id)
	return nil
}

func (w *World) disconnect(n *simNode) {
	w.hub.Disconnect(context.Background(), n.conn)
	n.conn = nil
	w.tracef("node %s offline", n.id)
}

// stepNode 让一台设备上线、下线，或执行一个待投递调用并回传结果。
func (w *World) stepNode(n *simNode) error {
	ctx := context.Background()
	if n.conn == nil {
		if w.faults && w.clock.Now().Before(n.awayUntil) {
			return nil // 长时间离线：调用转入后台（ADR-0035）
		}
		return w.connect(n)
	}
	if w.faults && w.chance(0.1) {
		w.disconnect(n)
		if w.chance(0.7) {
			n.awayUntil = w.clock.Now().Add(time.Duration(w.rng.IntN(300)) * time.Second)
		}
		return nil
	}
	pending, _, err := w.hub.Inbox.Pending(ctx, n.id)
	if err != nil || len(pending) == 0 {
		return err
	}
	inv := pending[w.rng.IntN(len(pending))]
	repeat := 1
	if w.faults && w.chance(0.2) {
		repeat = 2 // 重复投递
		w.Stats.Redeliveries++
	}
	for range repeat {
		res, crashed, err := w.execute(n, inv)
		if err != nil {
			return err
		}
		if crashed {
			w.Stats.NodeCrashes++
			w.tracef("node %s crashed executing %s", n.id, inv.GetCallId())
			w.disconnect(n)
			return nil
		}
		if err := w.hub.Result(ctx, n.id, res); err != nil {
			return err
		}
		w.tracef("node %s result %s", n.id, inv.GetCallId())
	}
	return nil
}

func (w *World) execute(n *simNode, inv *v1.Invoke) (*v1.InvokeResult, bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	crashed := false
	w.crash = func() { crashed = true; cancel() }
	w.currentCall = inv.GetCallId()
	defer func() { w.crash = nil; cancel() }()
	res, err := n.exec.Execute(ctx, inv)
	if crashed {
		return nil, true, nil
	}
	return res, false, err
}

// decide 对一个待审批的调用随机做出决定。
func (w *World) decide(approveAll bool) error {
	for _, sid := range w.live() {
		st, err := w.svc.Load(context.Background(), sid)
		if err != nil {
			return err
		}
		a := st.Active()
		if a == nil {
			continue
		}
		for _, c := range a.Calls {
			if c.Dispatched() && c.NodeID == askuser.NodeID {
				return w.answer(sid, c, approveAll)
			}
			if !c.AwaitingApproval() {
				continue
			}
			ok := approveAll || w.chance(0.7)
			if ok {
				w.Stats.Approvals++
			} else {
				w.Stats.Denials++
			}
			w.tracef("decide %s approved=%v", c.Call.GetCallId(), ok)
			return w.svc.Decide(context.Background(), sid, c.Call.GetCallId(), ok, "sim")
		}
	}
	return nil
}

// answer 在界面上回答一次 ask_user 提问：通常点选一个选项，有时给出不合法的回答（须被拒绝且不影响提问）。
// 打字回答走 submit：Run 等回答时提交的输入就是回答。
func (w *World) answer(sid string, c *session.Call, valid bool) error {
	q, err := askuser.Parse(c.Call.GetArgumentsJson())
	if err != nil {
		return fmt.Errorf("invariant: question %s was asked with invalid arguments: %w", c.Call.GetCallId(), err)
	}
	// 有时点卡片上的按钮：Run 等回答时，卡片动作就是回答（docs/design/cards.md §4）。同一动作已生效过时得到
	// duplicate，提问仍在等。
	if !valid && w.chance(0.7) {
		if in := w.pickCardAction(sid); in != nil {
			return w.submitInput(sid, in)
		}
	}
	a := &askuser.Answer{Selected: []string{q.Options[w.rng.IntN(len(q.Options))].ID}}
	if !valid && w.chance(0.2) {
		a.Selected = []string{"no-such-option"}
		err := w.svc.Answer(context.Background(), sid, c.Call.GetCallId(), a)
		if !errors.Is(err, service.ErrInvalid) {
			return fmt.Errorf("invariant: invalid answer to %s was not rejected: %v", c.Call.GetCallId(), err)
		}
		w.Stats.InvalidAnswers++
		w.tracef("answer %s rejected", c.Call.GetCallId())
		return nil
	}
	w.tracef("answer %s %v", c.Call.GetCallId(), a.Selected)
	return w.svc.Answer(context.Background(), sid, c.Call.GetCallId(), a)
}

// newWorker 创建第 slot 个 Worker：0 号只认领交互工作，长任务移交给其他 Worker（ADR-0029）。
func (w *World) newWorker(slot int) *runtime.Worker {
	w.nextW++
	id, pool := fmt.Sprintf("w%d", w.nextW), workqueue.Pool{}
	if slot == 0 {
		id, pool = fmt.Sprintf("%s%d", interactiveWorker, w.nextW), workqueue.Pool{MinPriority: workqueue.Interactive}
	}
	cfg := w.core.WorkerConfig(id, pool, nil)
	// Worker 的写入经 flakyLog；Service、Hub 与控制器用可靠的 Store。
	store := *w.store
	store.Log = flakyLog{Log: w.log, w: w}
	cfg.Store, cfg.Queue = &store, poolQueue{Queue: w.queue, w: w}
	cfg.Heartbeat = 0 // 续约按真实时间，模拟中关闭
	cfg.MaxTakeovers, cfg.MaxModelErrors, cfg.ApprovalTimeout, cfg.QuotaRecheck = 4, 3, approvalTimeout, time.Minute
	cfg.Notify, cfg.NotifyRunsAfter = simNotifier{w}, 30*time.Second
	return runtime.New(cfg)
}

func (w *World) chance(p float64) bool { return w.rng.Float64() < p }

func (w *World) tracef(format string, args ...any) {
	w.Trace = append(w.Trace, fmt.Sprintf("[%s] ", w.clock.Now().Format("15:04:05"))+fmt.Sprintf(format, args...))
}

// Run 执行完整模拟：先在故障与客户端动作下推进 Ticks 步，再停止注入并要求所有 Run 收敛到终态。
func (w *World) Run() error {
	for i := range w.opts.Ticks {
		if err := w.tick(); err != nil {
			return fmt.Errorf("tick %d: %w", i, err)
		}
		if err := w.CheckInvariants(); err != nil {
			return fmt.Errorf("tick %d: %w", i, err)
		}
	}
	return w.quiesce()
}

func (w *World) tick() error {
	err := w.act()
	// 外部写入的存储故障（flakyWaking）：客户端只看到错误，写入可能已经生效。设备与沙箱控制器不确认结果、
	// 稍后重发；审批与回答由用户重试（decide 会再次选到它）。
	var sf *storeFault
	if errors.As(err, &sf) {
		w.tracef("uncertain write: %v", err)
		return nil
	}
	return err
}

func (w *World) act() error {
	switch x := w.rng.Float64(); {
	case x < 0.47:
		return w.stepWorker(w.rng.IntN(len(w.workers)))
	case x < 0.50:
		if w.chance(0.1) {
			return w.janitors[0].Sweep(context.Background())
		}
		return w.stepJanitor(w.rng.IntN(len(w.janitors)))
	case x < 0.60:
		return w.stepNode(w.nodes[w.rng.IntN(len(w.nodes))])
	case x < 0.68:
		return w.stepController(w.rng.IntN(len(w.controllers)))
	case x < 0.70:
		if w.chance(0.2) {
			return w.setRelease(w.rng.IntN(len(simReleases)))
		}
		w.Stats.Reaps++
		return w.controllers[0].Reap(context.Background())
	case x < 0.76:
		return w.decide(false)
	case x < 0.82:
		if w.chance(0.25) {
			return w.callStep()
		}
		if w.chance(0.25) {
			return w.cancelBackground()
		}
		return w.submit()
	case x < 0.83:
		return w.presenceStep()
	case x < 0.84:
		if w.opts.LongRuns && !w.chance(0.1) {
			return nil // 长 Run 模式下少关、少删，否则 Run 很难跑满
		}
		switch x := w.rng.Float64(); {
		case x < 0.5:
			return w.closeSession()
		case x < 0.6:
			return w.deleteAccount()
		}
		return w.deleteSession()
	case x < 0.87:
		if w.opts.LongRuns && !w.chance(0.05) {
			return nil // 长 Run 模式下中断要少，否则 Run 很难跑满
		}
		return w.interrupt()
	case x < 0.94:
		if w.faults && w.chance(0.15) {
			return w.graceEdge() // 拨动时钟同样会使租约过期，只在注入故障时
		}
		// 大幅跳动时钟相当于 Worker 卡顿（GC、网络抖动）导致租约过期，属于故障。
		maxJump := 2 * deviceTimeout
		if !w.faults {
			maxJump = 2 * time.Second
		}
		d := time.Duration(w.rng.IntN(int(maxJump/time.Second))) * time.Second
		w.clock.Advance(d)
		w.tracef("clock +%s", d)
	default:
		if w.faults && w.chance(0.5) {
			i := w.rng.IntN(len(w.workers))
			if w.chance(0.3) {
				w.stopWorker(i)
				return nil
			}
			w.tracef("crash %s (idle)", w.workers[i].ID())
			w.restart(i)
		} else if w.faults {
			i := w.rng.IntN(len(w.controllers))
			w.tracef("crash %s (idle)", w.controllers[i].ID)
			w.Stats.ControllerCrashes++
			w.controllers[i] = w.newController()
		}
	}
	return nil
}

func (w *World) restart(i int) {
	w.Stats.Crashes++
	w.workers[i] = w.newWorker(i)
}

// stopWorker 优雅停机一个 Worker（如滚动发布）：移交它持有的 Run，再换上新的 Worker。
// 移交可能因存储故障失败，此时与崩溃相同，等租约过期后被接管。
func (w *World) stopWorker(i int) {
	w.Stats.GracefulStops++
	wk := w.workers[i]
	if err := wk.Stop(context.Background()); err != nil {
		w.tracef("stop %s: %v", wk.ID(), err)
	} else {
		w.tracef("stop %s", wk.ID())
	}
	w.workers[i] = w.newWorker(i)
}

func (w *World) stepWorker(i int) error {
	// 插入 Worker 步骤的钩子只针对客户端自己的写入：Worker 步骤中的写入（如委托时向子 Session 提交任务、
	// 把结果写回父 Session）不再插入步骤，否则会重入正在执行的 Worker。
	hook := w.afterEnqueue
	w.afterEnqueue = nil
	defer func() { w.afterEnqueue = hook }()
	ctx, cancel := context.WithCancel(context.Background())
	crashed := false
	w.crash = func() { crashed = true; cancel() }
	defer func() { w.crash = nil; cancel() }()

	wk := w.workers[i]
	did, err := wk.Step(ctx)
	if did {
		w.tracef("step %s", wk.ID())
	}
	if crashed {
		w.tracef("crash %s (mid-step)", wk.ID())
		w.restart(i)
		return nil
	}
	if err != nil {
		w.tracef("step %s: %v", wk.ID(), err)
		var me *modelError
		var sf *storeFault
		if errors.As(err, &me) || errors.Is(err, moderation.ErrUnavailable) || errors.As(err, &sf) || errors.Is(err, errDirectoryDown) {
			return nil
		}
		return fmt.Errorf("worker %s step: %w", wk.ID(), err)
	}
	return nil
}

func (w *World) submit() error {
	sid := w.sessions[w.rng.IntN(len(w.sessions))]
	input := model.TextBlocks(fmt.Sprintf("msg %d", w.rng.IntN(1000)))
	if a := w.cardAction(sid); a != nil {
		input = a
	} else if w.chance(0.1) {
		// 界面上下文：使上下文带有外部内容（Memory 写入闸门），并经过渲染与估算。
		w.Stats.UIContextInputs++
		input = append([]*v1.ContentBlock{{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{
			Screen: "详情", Content: strings.Repeat("c", w.rng.IntN(200))}}}}, input...)
	}
	return w.submitInput(sid, input)
}

// submitInput 提交一条输入（文字、带界面上下文的文字或卡片动作）：Run 等回答时它就是回答。检查去重与卡片动作的不变量。
func (w *World) submitInput(sid string, input []*v1.ContentBlock) error {
	w.afterEnqueue = func() error {
		if !w.chance(0.3) {
			return nil
		}
		w.Stats.EnqueueInterleavings++
		return w.stepWorker(w.rng.IntN(len(w.workers)))
	}
	defer func() { w.afterEnqueue = nil }()
	w.nextInput++
	inputID := fmt.Sprintf("in-%d", w.nextInput)
	res, err := w.svc.SubmitWithID(context.Background(), sid, inputID, input)
	var sf *storeFault
	if errors.As(err, &sf) && w.chance(0.5) {
		return w.retryUncertain(sid, inputID, input, sf.written)
	}
	if w.submitModerated(err) {
		w.tracef("submit %s rejected by moderation: %v", sid, err)
		return nil
	}
	if w.submitOverQuota(err) {
		w.tracef("submit %s rejected: %v", sid, err)
		return nil
	}
	if errors.Is(err, service.ErrConflict) {
		// 关闭的结果未知、重试也没有成功时（closeAt），模拟仍把 Session 当作打开的：提交得到 conflict 是对的。
		// 客户端由此得知它已关闭，再次关闭以补上关闭之后的步骤。
		if st, lerr := w.svc.Load(context.Background(), sid); lerr == nil && st.Closed != nil {
			w.tracef("submit %s: closed by an uncertain close", sid)
			return w.closeAt(slices.Index(w.sessions, sid), w.svc.Close(context.Background(), sid, "sim", "test"))
		}
	}
	if err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	if a := input[0].GetCardAction(); a != nil {
		w.Stats.CardActions++
		if !res.Duplicate {
			if w.cardInputs[sid] == nil {
				w.cardInputs[sid] = map[string]*v1.CardAction{}
			}
			w.cardInputs[sid][card.InputID(a)] = a
			if res.Answered != "" {
				w.Stats.CardAnswers++
			}
		}
		if res.Duplicate {
			w.Stats.CardDuplicates++ // 同一版卡片上的同一动作已经生效过：不写入
		} else if w.chance(0.5) {
			// 连点两次（另一台设备上也点了）：客户端的输入 ID 不同，平台按卡片推导的 ID 去重。
			again, err := w.svc.SubmitWithID(context.Background(), sid, inputID+"-again", input)
			if err == nil && !again.Duplicate {
				return fmt.Errorf("invariant: second click on card %s was not deduplicated: %+v", input[0].GetCardAction().GetCardId(), again)
			}
			if err == nil {
				w.Stats.CardDuplicates++
			}
		}
	}
	if res.Duplicate {
		w.tracef("submit %s → duplicate of %s", sid, res.RunID)
		return nil
	} else if res.Answered != "" {
		w.Stats.TypedAnswers++
	} else if res.Steered {
		w.Stats.Steers++
	} else {
		w.Stats.Runs++
		if err := w.checkNewRunVersion(sid); err != nil {
			return err
		}
	}
	w.tracef("submit %s → %s steered=%v", sid, res.RunID, res.Steered)
	if w.chance(0.2) {
		return w.retrySubmit(sid, inputID, input, res)
	}
	return nil
}

// retryUncertain 模拟"提交结果未知（存储故障）后以同一输入 ID 重试"：已生效的必须返回 duplicate，
// 没有生效的照常生效。
func (w *World) retryUncertain(sid, inputID string, input []*v1.ContentBlock, written bool) error {
	w.Stats.UncertainRetries++
	res, err := w.svc.SubmitWithID(context.Background(), sid, inputID, input)
	var sf *storeFault
	if errors.As(err, &sf) || (!written && (w.submitModerated(err) || w.submitOverQuota(err))) {
		w.tracef("retry %s: %v", inputID, err)
		return nil
	}
	if err != nil {
		if errors.Is(err, service.ErrNotFound) || errors.Is(err, service.ErrConflict) {
			return nil // 期间被删除或关闭（重试之间插入了 Worker 步骤），与去重无关
		}
		return fmt.Errorf("retry of uncertain input %s: %w", inputID, err)
	}
	if res.Duplicate != written {
		return fmt.Errorf("invariant: retry of input %s (written=%v) returned duplicate=%v", inputID, written, res.Duplicate)
	}
	w.tracef("retry %s → duplicate=%v", inputID, res.Duplicate)
	return nil
}

// retrySubmit 模拟"响应丢失后以同一输入 ID 重试"：其间可能有 Worker 推进了 Run；重试必须返回首次的结果，
// 且不写入任何事件（docs/design/m3-duplex-channel.md §5）。
func (w *World) retrySubmit(sid, inputID string, input []*v1.ContentBlock, first *service.SubmitResult) error {
	if w.chance(0.5) {
		if err := w.stepWorker(w.rng.IntN(len(w.workers))); err != nil {
			return err
		}
	}
	st, err := w.svc.Load(context.Background(), sid)
	if err != nil {
		return nil // 期间被删除：重试得到 not found，与去重无关
	}
	res, err := w.svc.SubmitWithID(context.Background(), sid, inputID, input)
	if err != nil {
		return fmt.Errorf("retry of input %s: %w", inputID, err)
	}
	after, err := w.svc.Load(context.Background(), sid)
	if err != nil {
		return err
	}
	if !res.Duplicate || res.RunID != first.RunID || res.Steered != first.Steered || res.Answered != first.Answered || after.Seq != st.Seq {
		return fmt.Errorf("invariant: retry of input %s returned %+v (first %+v), log %d→%d", inputID, res, first, st.Seq, after.Seq)
	}
	w.Stats.DuplicateInputs++
	w.tracef("retry %s → duplicate", inputID)
	return nil
}

func (w *World) interrupt() error {
	live := w.live()
	sid := live[w.rng.IntN(len(live))]
	st, err := w.svc.Load(context.Background(), sid)
	if err != nil {
		return err
	}
	a := st.Active()
	if a == nil {
		return nil
	}
	w.Stats.Interrupts++
	w.tracef("interrupt %s/%s", sid, a.ID)
	err = w.svc.Interrupt(context.Background(), sid, a.ID)
	var cf cancelFault
	if errors.As(err, &cf) {
		// 中断已写入，撤回委托失败：客户端看到错误，有时重试（重试会再撤回一次），有时放弃（子 Run 成为孤儿）。
		w.tracef("interrupt %s/%s: %v", sid, a.ID, err)
		if w.chance(0.5) {
			return nil
		}
		err = w.svc.Interrupt(context.Background(), sid, a.ID)
		if errors.As(err, &cf) {
			return nil
		}
	}
	return err
}

// quiesce 停止故障与客户端动作，持续推进直到所有 Run 终态。
func (w *World) quiesce() error {
	w.faults = false
	// 撤销配额（相当于调高配额）：因配额挂起的 Run 须在定期复查时恢复。
	w.quotas.Limits = nil
	// idleRounds 是所有 Janitor 连续无事可做的轮数。须超过一个租约 TTL（每轮 1 秒）才算清理队列已空：
	// 崩溃的 Janitor 持有的租约到期前，其他 Janitor 认领不到它的任务。
	idleRounds := 0
	for round := range 2000 {
		active := false
		if err := w.discoverChildren(); err != nil {
			return err
		}
		for _, sid := range w.live() {
			st, err := w.svc.Load(context.Background(), sid)
			if err != nil {
				return err
			}
			if st.Active() != nil {
				active = true
			}
		}
		done, err := w.lifecycleDone()
		if err != nil {
			return err
		}
		if !active && done && idleRounds > int(leaseTTL/time.Second) && w.checkBackgroundSettled() == nil {
			if err := w.CheckInvariants(); err != nil {
				return err
			}
			if err := w.checkDelegation(true); err != nil {
				return err
			}
			return w.checkDeleted(true)
		}
		for i := range w.workers {
			if err := w.stepWorker(i); err != nil {
				return fmt.Errorf("quiesce round %d: %w", round, err)
			}
		}
		for _, n := range w.nodes {
			if err := w.stepNode(n); err != nil {
				return fmt.Errorf("quiesce round %d: %w", round, err)
			}
		}
		for i := range w.controllers {
			if err := w.stepController(i); err != nil {
				return fmt.Errorf("quiesce round %d: %w", round, err)
			}
		}
		idleRounds++
		for i := range w.janitors {
			did, err := w.stepJanitorDid(i)
			if err != nil {
				return fmt.Errorf("quiesce round %d: %w", round, err)
			}
			if did {
				idleRounds = 0
			}
		}
		if round%10 == 0 {
			if err := w.janitors[0].Sweep(context.Background()); err != nil {
				return err
			}
		}
		if err := w.decide(true); err != nil {
			return err
		}
		w.clock.Advance(time.Second)
	}
	if err := w.checkBackgroundSettled(); err != nil {
		return err
	}
	return fmt.Errorf("liveness: runs did not reach a terminal state after faults stopped: %s", w.stuck())
}

// CheckInvariants 独立于写入方重新投影每个 Session 日志，并检查跨组件不变量。
func (w *World) CheckInvariants() error {
	if err := w.discoverChildren(); err != nil {
		return err
	}
	if err := w.checkDelegation(false); err != nil {
		return err
	}
	for _, sid := range w.known() {
		events, err := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			continue // 尚未创建（委托还没派发）或已删除的子 Session
		}
		st, err := session.Reduce(events)
		if err != nil {
			return fmt.Errorf("invariant: log of %s is invalid: %w", sid, err)
		}
		// 由快照加剩余日志恢复的投影必须与完整回放逐字节一致。
		loaded, err := w.store.Load(context.Background(), sid)
		if err != nil {
			return err
		}
		if !session.Equal(loaded, st) {
			return fmt.Errorf("invariant: snapshot-based load of %s diverges from full replay at seq %d", sid, st.Seq)
		}
		var pending error
		eachRun(events, func(r *session.Run) {
			if r.Status == session.RunCompleted && r.PendingCall() != nil && pending == nil {
				pending = fmt.Errorf("invariant: run %s completed with pending call", r.ID)
			}
		})
		if pending != nil {
			return pending
		}
		// 投影有界（延展性评审 §8.3）：保留明细的 Run 至多 KeepRuns+1 个，其余只剩摘要，摘要与 Run 合起来覆盖全部 Run。
		if len(st.Runs) > session.KeepRuns+1 {
			return fmt.Errorf("invariant: %s keeps %d runs in full, limit %d", sid, len(st.Runs), session.KeepRuns+1)
		}
		if requested := countPayload[*v1.Event_RunRequested](events); len(st.Summaries()) != requested {
			return fmt.Errorf("invariant: %s summarizes %d runs, log has %d", sid, len(st.Summaries()), requested)
		}
		for _, e := range events {
			// 判断目前只有影子模式：只记录，不影响执行（ADR-0030）。
			if d := e.GetDecisionMade(); d != nil && !d.GetShadow() {
				return fmt.Errorf("invariant: decision %s/%s in %s is not in shadow mode", d.GetKind(), d.GetSubject(), sid)
			}
			// 暂时性的目录故障只能经 StepErrorBudget（持续出错）使 Run 失败，不能一次就失败。
			if e.GetRunFailed().GetReason() == errDirectoryDown.Error() {
				return fmt.Errorf("invariant: run %s failed on a transient directory error", e.GetRunFailed().GetRunId())
			}
		}
		if err := checkInteractiveWorkers(events); err != nil {
			return fmt.Errorf("invariant: %s: %w", sid, err)
		}
		// 同一输入 ID 至多生效一次。
		inputs := map[string]*v1.Event{}
		for _, e := range events {
			id := e.GetRunRequested().GetInputId() + e.GetSteered().GetInputId() + e.GetToolResult().GetInputId()
			if id != "" && inputs[id] != nil {
				return fmt.Errorf("invariant: input %s applied twice in %s", id, sid)
			}
			inputs[id] = e
		}
		if err := checkCardActions(sid, events, inputs, w.cardInputs[sid]); err != nil {
			return err
		}
		if err := w.checkBackground(sid, events, st, slices.Contains(w.sessions, sid)); err != nil {
			return err
		}
	}
	if w.poolViolation != nil {
		return w.poolViolation
	}
	// 提问不经 Inbox（ADR-0025）：任何时候都不应有投给用户本人的待投递项。
	if items, _, err := w.hub.Inbox.Pending(context.Background(), askuser.NodeID); err != nil || len(items) > 0 {
		return fmt.Errorf("invariant: %d invocations queued for %s (err %v)", len(items), askuser.NodeID, err)
	}
	for id, n := range w.effects {
		if n > 1 {
			return fmt.Errorf("invariant: non-idempotent call %s executed %d times", id, n)
		}
	}
	if len(w.violations) > 0 {
		return fmt.Errorf("invariant: %s", w.violations[0])
	}
	if err := w.checkUsage(); err != nil {
		return err
	}
	if err := w.checkModeration(); err != nil {
		return err
	}
	return w.checkDeleted(false)
}

// checkCardActions 检查生效的卡片动作在日志中对客户端可见（docs/design/cards.md §5）：无论成为新 Run、插话还是
// 提问的回答，记下它的事件都带着规范化后的动作（CardActionOf），客户端据此把它显示为已选；否则再点得到 duplicate，
// 界面上却从没显示过它已执行。带动作的事件，其输入 ID 都由该动作推导，标签由平台填写。
func checkCardActions(sid string, events []*v1.Event, inputs map[string]*v1.Event, submitted map[string]*v1.CardAction) error {
	for _, e := range events {
		a := nodesdk.CardActionOf(e)
		if a == nil {
			continue
		}
		id := e.GetRunRequested().GetInputId() + e.GetSteered().GetInputId() + e.GetToolResult().GetInputId()
		if id != card.InputID(a) || a.GetLabel() == "" {
			return fmt.Errorf("invariant: card action %s/%s at seq %d in %s has input id %q, label %q", a.GetCallId(), a.GetAction(), e.GetSeq(), sid, id, a.GetLabel())
		}
	}
	for id, want := range submitted {
		e := inputs[id]
		if e == nil {
			return fmt.Errorf("invariant: card action %s/%s (input %s) took effect but is not in the log of %s", want.GetCallId(), want.GetAction(), id, sid)
		}
		if a := nodesdk.CardActionOf(e); a.GetCallId() != want.GetCallId() || a.GetAction() != want.GetAction() {
			return fmt.Errorf("invariant: input %s in %s at seq %d does not show card action %s/%s (got %v)", id, sid, e.GetSeq(), want.GetCallId(), want.GetAction(), a)
		}
	}
	return nil
}

// eachRun 重放 events，对每个 Run 调用一次 fn：终态的在进入终态时（此时仍在投影中，之后可能只剩摘要），
// 未终态的在最后。重放出错时停止。
func eachRun(events []*v1.Event, fn func(*session.Run)) {
	st := &session.State{}
	for _, e := range events {
		if err := st.Apply(e); err != nil {
			return
		}
		id := e.GetRunCompleted().GetRunId() + e.GetRunFailed().GetRunId() + e.GetRunInterrupted().GetRunId()
		if r := st.Run(id); id != "" && r != nil {
			fn(r)
		}
	}
	if a := st.Active(); a != nil {
		fn(a)
	}
}

func attemptKey(runID string, attempt uint32) string { return fmt.Sprintf("%s/%d", runID, attempt) }

// checkInteractiveWorkers 检查交互 Worker 不为长任务调用模型（ADR-0029）：Run 成为长任务（见 workqueue.Classify）
// 之后，交互 Worker 应在下一步之前移交它，而不是继续执行模型回合或上下文压缩。
func checkInteractiveWorkers(events []*v1.Event) error {
	st := &session.State{}
	workerOf := map[string]string{}
	for _, e := range events {
		var runID string
		var attempt uint32
		switch p := e.GetPayload().(type) {
		case *v1.Event_AttemptStarted:
			workerOf[attemptKey(p.AttemptStarted.GetRunId(), p.AttemptStarted.GetAttempt())] = p.AttemptStarted.GetWorkerId()
		case *v1.Event_AssistantMessage:
			runID, attempt = p.AssistantMessage.GetRunId(), p.AssistantMessage.GetAttempt()
		case *v1.Event_ContextCompacted:
			runID, attempt = p.ContextCompacted.GetRunId(), p.ContextCompacted.GetAttempt()
		}
		if wk := workerOf[attemptKey(runID, attempt)]; runID != "" && strings.HasPrefix(wk, interactiveWorker) &&
			st.WorkClass().Priority < workqueue.Interactive {
			return fmt.Errorf("interactive worker %s called the model for long run %s (seq %d)", wk, runID, e.GetSeq())
		}
		if err := st.Apply(e); err != nil {
			return err
		}
	}
	return nil
}

// CollectStats 从最终日志统计 Takeover、未知结果等路径的覆盖情况。
func (w *World) CollectStats() {
	for _, sid := range w.deleted {
		if t, err := w.deletions.Get(context.Background(), sid); err == nil && !t.CompletedAt.IsZero() {
			w.Stats.DeletionsCompleted++
		}
	}
	// 后台调用常跨越 Session 的关闭：已关闭的 Session 的日志一并统计。
	for _, sid := range w.known() {
		events, _ := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		w.countBackground(events)
	}
	for _, sid := range append(slices.Clone(w.sessions), w.children...) {
		events, _ := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		if st, err := session.Reduce(events); err == nil {
			w.Stats.PastRuns += len(st.Past)
			if st.Created.GetParent() != nil {
				w.Stats.Delegations++
			}
		}
		w.countDelegationResults(events)
		for _, e := range events {
			if d := e.GetDecisionMade(); d != nil {
				w.Stats.Decisions++
				if d.GetChosen() == "" {
					w.Stats.DecisionFallbacks++
				}
			}
		}
		eachRun(events, func(r *session.Run) {
			if r.Status == session.RunCompleted && r.Turns >= longRunTurns {
				w.Stats.LongRunsCompleted++
			}
			w.Stats.Takeovers += r.Takeovers
			for _, c := range r.Calls {
				if len(c.StartedAttempts) > 1 {
					w.Stats.Retried++
				}
			}
		})
		compacted := map[string]bool{}
		quotaSuspended := map[string]bool{}
		capOf := map[string]string{}
		for _, e := range events {
			switch p := e.GetPayload().(type) {
			case *v1.Event_MemoryRecalled:
				if len(p.MemoryRecalled.GetItems()) > 0 {
					w.Stats.Recalls++
				}
			case *v1.Event_ContextCompacted:
				w.Stats.Compactions++
				compacted[p.ContextCompacted.GetRunId()] = true
			case *v1.Event_AttemptStarted:
				if compacted[p.AttemptStarted.GetRunId()] {
					w.Stats.AttemptsAfterCompaction++
				}
			case *v1.Event_ToolResult:
				text := model.Text(p.ToolResult.GetContent())
				if p.ToolResult.GetIsError() && strings.HasPrefix(text, "outcome unknown") {
					w.Stats.OutcomeUnknown++
				}
				switch {
				case capOf[p.ToolResult.GetCallId()] == askuser.Capability && strings.HasPrefix(text, "timed out"):
					w.Stats.QuestionTimeouts++
				case capOf[p.ToolResult.GetCallId()] == askuser.Capability && p.ToolResult.GetAttempt() == 0:
					w.Stats.Answers++
				case p.ToolResult.GetAttempt() == 0:
					w.Stats.DeviceResults++
				}
				if strings.HasPrefix(text, "timed out") || strings.HasPrefix(text, "approval timed out") {
					w.Stats.Timeouts++
				}
			case *v1.Event_AgentSwitched:
				w.Stats.AgentSwitches++
			case *v1.Event_AssistantMessage:
				for _, tc := range p.AssistantMessage.GetToolCalls() {
					capOf[tc.GetCallId()] = tc.GetCapability()
				}
			case *v1.Event_ApprovalRequested:
				if capOf[p.ApprovalRequested.GetCallId()] == "memory_save" {
					w.Stats.MemoryApprovals++
				}
			case *v1.Event_RunSuspended:
				w.Stats.Suspensions++
				if p.RunSuspended.GetReason() == session.SuspendHandoff {
					w.Stats.Handoffs++
				} else if p.RunSuspended.GetReason() != "" {
					w.Stats.QuotaSuspensions++
					quotaSuspended[p.RunSuspended.GetRunId()] = true
				}
			case *v1.Event_RunCompleted:
				if quotaSuspended[p.RunCompleted.GetRunId()] {
					w.Stats.QuotaResumes++
				}
			case *v1.Event_RunFailed:
				if strings.HasSuffix(p.RunFailed.GetReason(), " turns") {
					w.Stats.TurnLimited++
				} else {
					w.Stats.Failed++
				}
			}
		}
	}
}

// Fingerprint 是全部日志的摘要，用于验证同一种子可复现。
func (w *World) Fingerprint() string {
	h := sha256.New()
	for _, sid := range append(slices.Clone(w.sessions), w.children...) {
		events, _ := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		for _, e := range events {
			b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(e)
			h.Write(b)
		}
		entries, _, _ := w.presence.Store.List(context.Background(), sid, w.clock.Now())
		for _, e := range entries {
			fmt.Fprintf(h, "%s|%s|%s|%v|%d|%d;", e.ConnID, e.DeviceID, e.Label, e.Focused, e.TypingUntil.Unix(), e.Expires.Unix())
		}
		w.fingerprintBackground(h, sid)
	}
	w.fingerprintInboxes(h)
	for i := range w.users {
		devices, _ := w.push.List(context.Background(), "bl", fmt.Sprintf("u%d", i))
		for _, d := range devices {
			fmt.Fprintf(h, "%s|%s|%d;", d.DeviceID, d.Token, d.LastActive.Unix())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// presenceStep 模拟会话客户端的在场写入：加入或更新、离开、续期。有时写向已删除的 Session——
// 对应"授权检查通过之后、写入之前 Session 被删除"的竞争，写入必须被撤回（ADR-0015）。
func (w *World) presenceStep() error {
	ctx := context.Background()
	sid := w.sessions[w.rng.IntN(len(w.sessions))]
	if len(w.deleted) > 0 && w.chance(0.2) {
		sid = w.deleted[w.rng.IntN(len(w.deleted))]
	}
	conn := fmt.Sprintf("conn%d", w.rng.IntN(3))
	now := w.clock.Now()
	switch x := w.rng.Float64(); {
	case x < 0.6:
		e := &presence.Entry{SessionID: sid, ConnID: conn, DeviceID: "dev-" + conn, Label: "phone", Kind: "mobile",
			Focused: w.chance(0.5), Expires: now.Add(presence.TTL)}
		if w.chance(0.3) {
			e.TypingUntil = now.Add(presence.TypingFor)
		}
		gone, err := lifecycle.Deleted(ctx, w.deletions, sid)
		if err != nil {
			return err
		}
		if err := w.presence.Put(ctx, e); err != nil {
			return fmt.Errorf("presence put: %w", err)
		}
		w.Stats.PresenceWrites++
		if gone {
			if entries, _, err := w.presence.Store.List(ctx, sid, time.Time{}); err != nil || len(entries) > 0 {
				return fmt.Errorf("invariant: presence written to deleted session %s was not withdrawn (err %v)", sid, err)
			}
			w.Stats.PresenceWithdrawn++
		}
		w.tracef("presence put %s %s", sid, conn)
	case x < 0.8:
		w.tracef("presence remove %s %s", sid, conn)
		return w.presence.Store.Remove(ctx, sid, conn)
	default:
		w.tracef("presence touch %s", conn)
		return w.presence.Store.Touch(ctx, conn, now.Add(presence.TTL))
	}
	return nil
}

func (w *World) echoCap() capability.Capability {
	return capability.Func{
		S: capability.Spec{Name: "echo", Idempotent: true, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
			if w.faults && w.chance(0.1) {
				w.crash()
				return nil, ctx.Err()
			}
			return model.TextBlocks(inv.Arguments), nil
		},
	}
}

// sendCap 模拟有副作用的调用：先产生效果，再可能在返回结果前崩溃。
func (w *World) sendCap() capability.Capability {
	return capability.Func{
		S: capability.Spec{Name: "send", Idempotent: false, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
			w.effects[inv.SessionID+"/"+inv.CallID]++
			if w.faults && w.chance(0.2) {
				w.crash()
				return nil, ctx.Err()
			}
			return model.TextBlocks("sent"), nil
		},
	}
}

type modelError struct{ overflow bool }

func (e *modelError) Error() string {
	if e.overflow {
		return "simulated context overflow"
	}
	return "simulated model error"
}

// Is 让模拟的超长错误同时满足 errors.Is(err, model.ErrContextOverflow)。
func (e *modelError) Is(target error) bool { return e.overflow && target == model.ErrContextOverflow }

// checkRequest 检查发给模型的请求：估算大小不超过窗口；每个调用请求之后紧跟其全部结果，且没有孤立的结果。
func checkRequest(req *model.Request) error {
	if n := model.EstimateRequest(req); n > simContext.Window {
		return fmt.Errorf("model request of ~%d tokens exceeds window %d", n, simContext.Window)
	}
	var open []string
	for i, m := range req.Messages {
		switch {
		case m.Role == model.RoleTool:
			if len(open) == 0 || open[0] != m.ToolCallID {
				return fmt.Errorf("message %d: tool result %s without matching call", i, m.ToolCallID)
			}
			open = open[1:]
		case len(open) > 0:
			return fmt.Errorf("message %d: %s before results of %v", i, m.Role, open)
		}
		for _, tc := range m.ToolCalls {
			open = append(open, tc.GetCallId())
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("request ends with calls %v lacking results", open)
	}
	return nil
}

// pad 生成随机长度的负载，使上下文持续增长。
func (w *World) pad() string { return strings.Repeat("p", w.rng.IntN(400)) }

// simModel 是由 World 随机源驱动的脚本化模型。
type simModel struct{ w *World }

func (m *simModel) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	w := m.w
	if runtime.IsSummaryRequest(req) {
		return m.summarize(ctx, req)
	}
	if err := checkRequest(req); err != nil {
		w.violations = append(w.violations, err.Error())
	}
	if w.faults && w.chance(0.02) {
		w.Stats.Overflows++
		return nil, &modelError{overflow: true}
	}
	if w.faults {
		switch {
		case w.chance(0.05):
			w.Stats.ModelErrors++
			return nil, &modelError{}
		case w.chance(0.03):
			w.crash()
			return nil, ctx.Err()
		}
	}
	// 自最后一条用户消息以来的助手轮数，用于让对话收敛。
	turns := 0
	for _, msg := range req.Messages {
		switch msg.Role {
		case model.RoleUser:
			turns = 0
		case model.RoleAssistant:
			turns++
		}
	}
	resp := &model.Response{Model: "sim/m", Usage: &v1.Usage{InputTokens: uint64(model.EstimateRequest(req)), OutputTokens: 20}}
	more := turns < 3 && w.chance(0.6)
	if w.opts.LongRuns {
		more = turns < 100 && w.chance(0.97)
	}
	if more {
		tools := req.Tools
		if w.opts.LongRuns {
			// 长 Run 模式下不委托：子 Run 同样要跑上百轮，层层委托使收敛远超模拟的时长。
			tools = slices.DeleteFunc(slices.Clone(tools), func(t model.ToolSpec) bool { return strings.HasPrefix(t.Name, "agent__") })
		}
		// 设备上的能力只是十几个工具中的两个：偏向它们，使调用离线设备、转入后台（ADR-0035）足够常见。
		var devices []string
		for _, t := range tools {
			if strings.HasPrefix(t.Name, "pc__") {
				devices = append(devices, t.Name)
			}
		}
		for i := range 1 + w.rng.IntN(2) {
			name := tools[w.rng.IntN(len(tools))].Name
			if len(devices) > 0 && w.chance(0.3) {
				name = devices[w.rng.IntN(len(devices))]
			}
			id := fmt.Sprintf("call_%d", i) // 故意跨轮重复，检验运行时的 ID 改写
			if w.chance(0.3) {
				id = ""
			}
			resp.ToolCalls = append(resp.ToolCalls, &v1.ToolCall{CallId: id, Capability: name, ArgumentsJson: w.toolArgs(name)})
		}
		// 提问之前有时先展示一张卡片：用户可能在等回答时点卡片上的按钮，这个动作就是回答（docs/design/cards.md §4）。
		// 否则同一 Session 里很少既有卡片又有待回答的提问。
		if c := resp.ToolCalls[0]; len(resp.ToolCalls) == 1 && c.GetCapability() == askuser.Capability && w.chance(0.7) &&
			slices.ContainsFunc(tools, func(t model.ToolSpec) bool { return t.Name == card.Capability }) {
			resp.ToolCalls = []*v1.ToolCall{{Capability: card.Capability, ArgumentsJson: w.toolArgs(card.Capability)}, c}
		}
		return resp, nil
	}
	text := fmt.Sprintf("reply after %d messages", len(req.Messages))
	if onDelta != nil {
		onDelta(model.Delta{Text: text})
	}
	resp.Content = model.TextBlocks(text)
	return resp, nil
}

// summarize 是脚本化的摘要模型：可能报错、崩溃或报告超长，否则返回简短摘要。
func (m *simModel) summarize(ctx context.Context, req *model.Request) (*model.Response, error) {
	w := m.w
	if n := model.EstimateRequest(req); n > simContext.Window {
		w.violations = append(w.violations, fmt.Sprintf("summary request of ~%d tokens exceeds window %d", n, simContext.Window))
	}
	if w.faults {
		switch x := w.rng.Float64(); {
		case x < 0.05:
			w.Stats.SummaryFaults++
			return nil, &modelError{}
		case x < 0.08:
			w.Stats.SummaryFaults++
			w.crash()
			return nil, ctx.Err()
		case x < 0.10:
			w.Stats.SummaryFaults++
			return nil, &modelError{overflow: true}
		}
	}
	text := fmt.Sprintf("summary of %d bytes", len(model.Text(req.Messages[0].Content)))
	return &model.Response{Model: "sim/m", Content: model.TextBlocks(text), Usage: &v1.Usage{InputTokens: uint64(model.EstimateRequest(req))}}, nil
}

// toolArgs 为工具生成参数：Memory 工具需要合法参数才能写入，其余工具用随机长度的负载使上下文增长。
func (w *World) toolArgs(name string) string {
	switch name {
	case "memory_save":
		w.Stats.MemorySaves++
		return fmt.Sprintf(`{"category":"preference","content":"偏好 %d"}`, w.rng.IntN(50))
	case "memory_search":
		return `{"query":"偏好"}`
	case askuser.Capability:
		if w.chance(0.1) {
			return `{"question":"只有一个选项？","options":[{"id":"a","label":"甲"}]}` // 不合法：交还模型改正
		}
		w.Stats.Questions++
		return fmt.Sprintf(`{"question":"选哪个？%s","options":[{"id":"a","label":"甲"},{"id":"b","label":"乙"},{"id":"c","label":"丙"}]}`, strings.Repeat("p", w.rng.IntN(50)))
	case "memory_forget":
		return fmt.Sprintf(`{"id":"mem_call_id-%d"}`, w.rng.IntN(500))
	case card.Capability:
		if w.chance(0.1) {
			return `{"title":""}` // 不合法：错误结果交还模型
		}
		w.Stats.Cards++
		// card_id 取自很小的集合：同一 ID 再次展示即替换前一版，旧版本上的动作与新版本的互不混淆。负载放在正文里：
		// 标题有长度上限，放在标题里时多数卡片不合法，就很少有卡片可点。
		return fmt.Sprintf(`{"card_id":"c%d","title":"卡片","body":[{"type":"markdown","text":"%s"}],"buttons":[{"id":"ok","label":"好"},{"id":"no","label":"不"}]}`,
			w.rng.IntN(3), "p"+w.pad())
	case "agent__sim":
		if w.chance(0.1) {
			return `{}` // 没有 task：不派发，错误交还模型
		}
		return fmt.Sprintf(`{"task":"子任务 %d"}`, w.rng.IntN(100))
	}
	return `{"pad":"` + w.pad() + `"}`
}

// countPayload 返回 events 中载荷为 T 的事件数。
func countPayload[T any](events []*v1.Event) int {
	n := 0
	for _, e := range events {
		if _, ok := e.GetPayload().(T); ok {
			n++
		}
	}
	return n
}

// stuck 描述仍未终态的 Run 停在哪里，供活性失败时定位。
func (w *World) stuck() string {
	var out []string
	for _, sid := range w.live() {
		st, err := w.store.Load(context.Background(), sid)
		if err != nil || st.Active() == nil {
			continue
		}
		a := st.Active()
		desc := fmt.Sprintf("%s/%s %s turns=%d", sid, a.ID, a.Status, a.Turns)
		if c := a.PendingCall(); c != nil {
			desc += fmt.Sprintf(" pending %s on %q approval=%v", c.Call.GetCapability(), c.NodeID, c.Approval != nil)
		}
		out = append(out, desc)
	}
	return strings.Join(out, "; ")
}

// simCardCap 是 show_card，工具声明缩短：模拟的上下文窗口很小（simContext），完整的卡片 Schema 会挤占压缩的余地。
// 执行路径与真实能力相同（card.New）。
func simCardCap() capability.Capability {
	c := card.New(nil)
	spec := c.Spec()
	spec.Description, spec.InputSchema = "card", json.RawMessage(`{"type":"object"}`)
	return capability.Func{S: spec, Fn: c.Invoke}
}

// cardAction 有时返回对 Session 最近展示过的某张卡片的动作（pickCardAction）。
func (w *World) cardAction(sid string) []*v1.ContentBlock {
	if !w.chance(0.5) {
		return nil
	}
	return w.pickCardAction(sid)
}

// pickCardAction 返回对 Session 最近展示过的某张卡片的动作（点按钮），没有卡片时返回 nil：有时不带事件序号，迫使平台
// 扫描日志。
func (w *World) pickCardAction(sid string) []*v1.ContentBlock {
	st, err := w.svc.Load(context.Background(), sid)
	if err != nil {
		return nil
	}
	var cards []*v1.Event
	for _, e := range st.History {
		if card.Find(e.GetToolResult().GetContent()) != nil {
			cards = append(cards, e)
		}
	}
	if len(cards) == 0 {
		return nil
	}
	e := cards[w.rng.IntN(len(cards))]
	c := card.Find(e.GetToolResult().GetContent())
	a := &v1.CardAction{CardId: c.GetCardId(), CallId: c.GetCallId(), Action: []string{"ok", "no"}[w.rng.IntN(2)]}
	if w.chance(0.7) {
		a.Seq = e.GetSeq()
	}
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: a}}}
}
