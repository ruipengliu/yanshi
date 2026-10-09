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
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/janitor"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
	"yanshi/sdk/nodesdk"
)

// Stores 是模拟使用的存储实现；默认为内存实现。
type Stores struct {
	Log   eventlog.Log
	Queue workqueue.Queue
	Dir   node.Directory
	Inbox node.Inbox
	// 沙箱控制器的队列与共享状态
	SandboxQueue workqueue.Queue
	Ledger       nodesdk.Ledger
	Activity     sandbox.Activity
	// Session 生命周期：索引、删除记录与清理队列
	Index        lifecycle.Index
	Deletions    lifecycle.Deletions
	JanitorQueue workqueue.Queue
	// Memory（docs/design/m4-memory-grant.md）
	Memory memory.Store
	Grants memory.Grants
	// 投影快照（docs/design/m2-scale-test.md §6）
	Snapshots session.Snapshots
	// 用量（docs/design/m4-quota-usage.md）
	Usage usage.Store
}

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
	// EnqueueInterleavings 是在客户端入队与写日志之间插入 Worker 步骤的次数。
	EnqueueInterleavings int
	// 配额：因配额挂起的次数、提交时被拒绝的次数、挂起后恢复并结束的 Run；注销账号的次数。
	QuotaSuspensions, QuotaRejections, QuotaResumes, AccountDeletions int
}

// hookQueue 在 Enqueue 成功后调用 after。
type hookQueue struct {
	workqueue.Queue
	after func() error
}

func (q *hookQueue) Enqueue(ctx context.Context, sessionID string) error {
	if err := q.Queue.Enqueue(ctx, sessionID); err != nil {
		return err
	}
	return q.after()
}

// simContext 是模拟 AgentDef 的上下文配置：窗口很小（工具声明约占 650），使压缩频繁发生。
var simContext = agentdef.Context{Window: 2000, CompactAt: 0.75, KeepRecent: 300, MaxToolResult: 250}

const longRunTurns = 20

const (
	leaseTTL        = 10 * time.Second
	approvalTimeout = 30 * time.Second
	deviceTimeout   = 60 * time.Second
)

// simNode 是一台模拟设备：SDK 的 Executor 与 Ledger 跨崩溃保留，连接随上下线变化。
type simNode struct {
	id     string
	hello  *v1.Hello
	exec   *nodesdk.Executor
	ledger *nodesdk.MemLedger
	conn   *node.Conn
}

type World struct {
	opts    Options
	rng     *rand.Rand
	clock   *clock.Fake
	log     eventlog.Log
	store   *session.Store
	queue   workqueue.Queue
	svc     *service.Service
	agents  *agentdef.Registry
	catalog *capability.Catalog
	hub     *node.Hub
	router  *sandbox.Router
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
	usage        usage.Store
	quotas       *usage.Quotas
	meter        *usage.Meter
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
	// effects 记录非幂等 Capability（进程内与设备上）每个调用 ID 的实际执行次数。
	effects map[string]int
	// crash 在一次 Step 或设备执行期间可用：调用它模拟该进程在此刻崩溃。
	crash func()
	// currentCall 是设备正在执行的调用 ID。
	currentCall string
	faults      bool
	// afterEnqueue 非空时，在客户端动作内部的每次入队之后调用。
	afterEnqueue func() error
	// violations 记录脚本化模型观察到的请求级不变量违反（超出窗口、调用配对残缺）。
	violations []string
	Stats      Stats
	// Trace 记录每一步的动作，失败时用于定位。
	Trace []string
}

func New(opts Options) (*World, error) {
	w := &World{
		opts:    opts,
		rng:     rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15)),
		clock:   clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		effects: map[string]int{},
		faults:  opts.Faults,
	}
	stores := Stores{
		Log: memlog.New(), Queue: memqueue.New(w.clock), Dir: node.NewMemDirectory(w.clock), Inbox: node.NewMemInbox(),
		SandboxQueue: memqueue.New(w.clock), Ledger: nodesdk.NewMemLedger(), Activity: sandbox.NewMemActivity(),
		Index: lifecycle.NewMemIndex(), Deletions: lifecycle.NewMemDeletions(), JanitorQueue: memqueue.New(w.clock),
		Memory: memory.NewMemStore(), Grants: memory.NewMemGrants(), Snapshots: session.NewMemSnapshots(),
		Usage: usage.NewMem(),
	}
	if opts.NewStores != nil {
		stores = opts.NewStores(w.clock)
	}
	w.log, w.queue = stores.Log, stores.Queue
	// 快照每 5 条写一次，使加载几乎总是走"快照 + 剩余日志"的路径。
	w.store = &session.Store{Log: w.log, IDs: ids.Sequential("id"), Clock: w.clock, Snapshots: stores.Snapshots,
		SnapshotEvery: 5, Deletions: stores.Deletions}
	maxTurns := 6
	if opts.LongRuns {
		maxTurns = 120
	}
	agents, err := agentdef.NewRegistry(&agentdef.Def{
		Name: "sim", Version: "1", Model: "sim/m",
		Capabilities: []string{"echo", "send", "memory_save", "memory_forget", "memory_search", "device:*", "sandbox:*"},
		MaxTurns:     maxTurns, Context: simContext, Memory: agentdef.MemoryConfig{Recall: 3},
	})
	if err != nil {
		return nil, err
	}
	w.agents = agents
	w.memories = &memory.Service{Store: stores.Memory, Grants: stores.Grants, Deletions: stores.Deletions, Clock: w.clock,
		IDs: ids.Sequential("mem"), MaxPerUser: 20}
	w.catalog = &capability.Catalog{
		Local: capability.NewRegistry(append([]capability.Capability{w.echoCap(), w.sendCap()}, memory.Capabilities(w.memories)...)...),
		Nodes: stores.Dir, DefaultTimeout: deviceTimeout,
		Sandbox: &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID},
	}
	w.index, w.deletions, w.janitorQueue = stores.Index, stores.Deletions, stores.JanitorQueue
	w.usage = &checkedUsage{Store: stores.Usage, w: w}
	w.quotas = &usage.Quotas{Store: stores.Usage, Limits: simLimits(opts.LongRuns)}
	w.meter = &usage.Meter{Store: w.usage, Prices: simPrices, Deletions: w.deletions}
	w.hub = &node.Hub{Dir: stores.Dir, Inbox: stores.Inbox, Store: w.store, Queue: w.queue, Auth: node.InsecureDevAuth{}, Deletions: w.deletions}
	w.sandboxQueue, w.ledger, w.activity = stores.SandboxQueue, stores.Ledger, stores.Activity
	w.router = &sandbox.Router{Hub: w.hub, Queue: w.sandboxQueue}
	w.artifacts = &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: artifact.NewMemBlobs(), IDs: ids.Sequential("art"), Clock: w.clock, Deletions: w.deletions}
	w.provider = sandbox.NewFake()
	w.provider.ExecFunc = w.sandboxExec
	// Service 的入队可能在写日志之前或之后，模拟在入队之后插入 Worker 的一步，
	// 以覆盖"入队、认领、写日志、释放"的交错（丢失唤醒）。
	w.svc = &service.Service{Store: w.store, Queue: &hookQueue{Queue: w.queue, after: func() error {
		if w.afterEnqueue == nil {
			return nil
		}
		return w.afterEnqueue()
	}}, Agents: agents, Nodes: w.router, Index: w.index, Deletions: w.deletions, Janitor: w.janitorQueue, Quotas: w.quotas}
	for range 2 {
		w.controllers = append(w.controllers, w.newController())
	}
	for range 2 {
		w.janitors = append(w.janitors, w.newJanitor())
	}
	for range opts.Workers {
		w.workers = append(w.workers, w.newWorker())
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
	return &sandbox.Controller{
		ID: fmt.Sprintf("c%d", w.nextC), Queue: w.sandboxQueue, Hub: w.hub, Provider: w.provider,
		Activity: w.activity, Ledger: w.ledger, Clock: w.clock, LeaseTTL: leaseTTL, IdleTTL: 2 * time.Minute,
		Artifacts: w.artifacts, Lifecycle: &lifecycle.Guard{Index: w.index, Deletions: w.deletions}, Meter: w.meter,
	}
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
	n.hello = &v1.Hello{NodeId: n.id, BusinessLine: "bl", EndUser: fmt.Sprintf("u%d", i), Label: "pc", Kind: "desktop"}
	effect := func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
		w.effects[n.id+"/"+w.currentCall]++
		if w.faults && w.chance(0.2) {
			w.crash() // App 在产生副作用后、记录结果前被杀
			return nil, ctx.Err()
		}
		return model.TextBlocks("done"), nil
	}
	read := func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
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
		return w.connect(n)
	}
	if w.faults && w.chance(0.1) {
		w.disconnect(n)
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
	for _, sid := range w.sessions {
		st, err := w.svc.Load(context.Background(), sid)
		if err != nil {
			return err
		}
		a := st.Active()
		if a == nil {
			continue
		}
		for _, c := range a.Calls {
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

func (w *World) newWorker() *runtime.Worker {
	w.nextW++
	gw := model.NewGateway()
	gw.Register("sim", &simModel{w: w})
	return runtime.New(runtime.Config{
		ID: fmt.Sprintf("w%d", w.nextW), Store: w.store, Queue: w.queue, Agents: w.agents,
		Model: gw, Catalog: w.catalog, Dispatch: w.router, LeaseTTL: leaseTTL,
		MaxTakeovers: 4, MaxModelErrors: 3, ApprovalTimeout: approvalTimeout, Memory: w.memories,
		Meter: w.meter, Quotas: w.quotas, QuotaRecheck: time.Minute,
	})
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
		w.Stats.Reaps++
		return w.controllers[0].Reap(context.Background())
	case x < 0.76:
		return w.decide(false)
	case x < 0.83:
		return w.submit()
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
	w.workers[i] = w.newWorker()
}

func (w *World) stepWorker(i int) error {
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
		if errors.As(err, &me) {
			return nil
		}
		return fmt.Errorf("worker %s step: %w", wk.ID(), err)
	}
	return nil
}

func (w *World) submit() error {
	sid := w.sessions[w.rng.IntN(len(w.sessions))]
	w.afterEnqueue = func() error {
		if !w.chance(0.3) {
			return nil
		}
		w.Stats.EnqueueInterleavings++
		return w.stepWorker(w.rng.IntN(len(w.workers)))
	}
	defer func() { w.afterEnqueue = nil }()
	res, err := w.svc.Submit(context.Background(), sid, model.TextBlocks(fmt.Sprintf("msg %d", w.rng.IntN(1000))))
	if w.submitOverQuota(err) {
		w.tracef("submit %s rejected: %v", sid, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	if res.Steered {
		w.Stats.Steers++
	} else {
		w.Stats.Runs++
	}
	w.tracef("submit %s → %s steered=%v", sid, res.RunID, res.Steered)
	return nil
}

func (w *World) interrupt() error {
	sid := w.sessions[w.rng.IntN(len(w.sessions))]
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
	return w.svc.Interrupt(context.Background(), sid, a.ID)
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
		for _, sid := range w.sessions {
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
		if !active && done && idleRounds > int(leaseTTL/time.Second) {
			if err := w.CheckInvariants(); err != nil {
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
	return errors.New("liveness: runs did not reach a terminal state after faults stopped")
}

// CheckInvariants 独立于写入方重新投影每个 Session 日志，并检查跨组件不变量。
func (w *World) CheckInvariants() error {
	for _, sid := range append(slices.Clone(w.sessions), w.closed...) {
		events, err := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		if err != nil {
			return err
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
		for _, r := range st.Runs {
			if r.Status == session.RunCompleted && r.PendingCall() != nil {
				return fmt.Errorf("invariant: run %s completed with pending call", r.ID)
			}
		}
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
	return w.checkDeleted(false)
}

// CollectStats 从最终日志统计 Takeover、未知结果等路径的覆盖情况。
func (w *World) CollectStats() {
	for _, sid := range w.deleted {
		if t, err := w.deletions.Get(context.Background(), sid); err == nil && !t.CompletedAt.IsZero() {
			w.Stats.DeletionsCompleted++
		}
	}
	for _, sid := range w.sessions {
		events, _ := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		if st, err := session.Reduce(events); err == nil {
			for _, r := range st.Runs {
				if r.Status == session.RunCompleted && r.Turns >= longRunTurns {
					w.Stats.LongRunsCompleted++
				}
				w.Stats.Takeovers += r.Takeovers
				for _, c := range r.Calls {
					if len(c.StartedAttempts) > 1 {
						w.Stats.Retried++
					}
				}
			}
		}
		compacted := map[string]bool{}
		quotaSuspended := map[string]bool{}
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
				if p.ToolResult.GetAttempt() == 0 {
					w.Stats.DeviceResults++
				}
				if strings.HasPrefix(text, "timed out") || strings.HasPrefix(text, "approval timed out") {
					w.Stats.Timeouts++
				}
			case *v1.Event_RunSuspended:
				w.Stats.Suspensions++
				if p.RunSuspended.GetReason() != "" {
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
	for _, sid := range w.sessions {
		events, _ := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		for _, e := range events {
			b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(e)
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
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
		for i := range 1 + w.rng.IntN(2) {
			name := req.Tools[w.rng.IntN(len(req.Tools))].Name
			id := fmt.Sprintf("call_%d", i) // 故意跨轮重复，检验运行时的 ID 改写
			if w.chance(0.3) {
				id = ""
			}
			resp.ToolCalls = append(resp.ToolCalls, &v1.ToolCall{CallId: id, Capability: name, ArgumentsJson: w.toolArgs(name)})
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
	case "memory_forget":
		return fmt.Sprintf(`{"id":"mem_call_id-%d"}`, w.rng.IntN(500))
	}
	return `{"pad":"` + w.pad() + `"}`
}
