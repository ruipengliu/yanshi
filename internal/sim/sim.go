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
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
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
}

const (
	leaseTTL        = 10 * time.Second
	approvalTimeout = 30 * time.Second
	deviceTimeout   = 60 * time.Second
)

// simNode 是一台模拟设备：SDK 的 Executor 与 Ledger 跨崩溃保留，连接随上下线变化。
type simNode struct {
	id    string
	hello *v1.Hello
	exec  *nodesdk.Executor
	conn  *node.Conn
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
	ledger       nodesdk.Ledger
	activity     sandbox.Activity
	controllers  []*sandbox.Controller
	nextC        int
	workers      []*runtime.Worker
	nextW        int

	sessions []string
	// effects 记录非幂等 Capability（进程内与设备上）每个调用 ID 的实际执行次数。
	effects map[string]int
	// crash 在一次 Step 或设备执行期间可用：调用它模拟该进程在此刻崩溃。
	crash func()
	// currentCall 是设备正在执行的调用 ID。
	currentCall string
	faults      bool
	Stats       Stats
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
	}
	if opts.NewStores != nil {
		stores = opts.NewStores(w.clock)
	}
	w.log, w.queue = stores.Log, stores.Queue
	w.store = &session.Store{Log: w.log, IDs: ids.Sequential("id"), Clock: w.clock}
	agents, err := agentdef.NewRegistry(&agentdef.Def{
		Name: "sim", Version: "1", Model: "sim/m", Capabilities: []string{"echo", "send", "device:*", "sandbox:*"}, MaxTurns: 6,
	})
	if err != nil {
		return nil, err
	}
	w.agents = agents
	w.catalog = &capability.Catalog{
		Local: capability.NewRegistry(w.echoCap(), w.sendCap()), Nodes: stores.Dir, DefaultTimeout: deviceTimeout,
		Sandbox: &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID},
	}
	w.hub = &node.Hub{Dir: stores.Dir, Inbox: stores.Inbox, Store: w.store, Queue: w.queue, Auth: node.InsecureDevAuth{}}
	w.sandboxQueue, w.ledger, w.activity = stores.SandboxQueue, stores.Ledger, stores.Activity
	w.router = &sandbox.Router{Hub: w.hub, Queue: w.sandboxQueue}
	w.provider = sandbox.NewFake()
	w.provider.ExecFunc = w.sandboxExec
	w.svc = &service.Service{Store: w.store, Queue: w.queue, Agents: agents, Nodes: w.router}
	for range 2 {
		w.controllers = append(w.controllers, w.newController())
	}
	for range opts.Workers {
		w.workers = append(w.workers, w.newWorker())
	}
	for i := range opts.Sessions {
		id, err := w.svc.Create(context.Background(), service.CreateRequest{
			BusinessLine: "bl", EndUser: fmt.Sprintf("u%d", i), Agent: "sim",
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
	n.exec = nodesdk.NewExecutor(nodesdk.NewMemLedger(),
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
		MaxTakeovers: 4, MaxModelErrors: 3, ApprovalTimeout: approvalTimeout,
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
	case x < 0.50:
		return w.stepWorker(w.rng.IntN(len(w.workers)))
	case x < 0.60:
		return w.stepNode(w.nodes[w.rng.IntN(len(w.nodes))])
	case x < 0.68:
		return w.stepController(w.rng.IntN(len(w.controllers)))
	case x < 0.70:
		w.Stats.Reaps++
		return w.controllers[0].Reap(context.Background())
	case x < 0.76:
		return w.decide(false)
	case x < 0.84:
		return w.submit()
	case x < 0.87:
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
	res, err := w.svc.Submit(context.Background(), sid, model.TextBlocks(fmt.Sprintf("msg %d", w.rng.IntN(1000))))
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
		if !active {
			return w.CheckInvariants()
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
		if err := w.decide(true); err != nil {
			return err
		}
		w.clock.Advance(time.Second)
	}
	return errors.New("liveness: runs did not reach a terminal state after faults stopped")
}

// CheckInvariants 独立于写入方重新投影每个 Session 日志，并检查跨组件不变量。
func (w *World) CheckInvariants() error {
	for _, sid := range w.sessions {
		events, err := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		if err != nil {
			return err
		}
		st, err := session.Reduce(events)
		if err != nil {
			return fmt.Errorf("invariant: log of %s is invalid: %w", sid, err)
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
	return nil
}

// CollectStats 从最终日志统计 Takeover、未知结果等路径的覆盖情况。
func (w *World) CollectStats() {
	for _, sid := range w.sessions {
		events, _ := eventlog.ReadAll(context.Background(), w.log, sid, 0)
		if st, err := session.Reduce(events); err == nil {
			for _, r := range st.Runs {
				w.Stats.Takeovers += r.Takeovers
				for _, c := range r.Calls {
					if len(c.StartedAttempts) > 1 {
						w.Stats.Retried++
					}
				}
			}
		}
		for _, e := range events {
			switch p := e.GetPayload().(type) {
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

type modelError struct{}

func (*modelError) Error() string { return "simulated model error" }

// simModel 是由 World 随机源驱动的脚本化模型。
type simModel struct{ w *World }

func (m *simModel) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	w := m.w
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
	resp := &model.Response{Model: "sim/m", Usage: &v1.Usage{InputTokens: uint64(len(req.Messages))}}
	if turns < 3 && w.chance(0.6) {
		for i := range 1 + w.rng.IntN(2) {
			name := req.Tools[w.rng.IntN(len(req.Tools))].Name
			id := fmt.Sprintf("call_%d", i) // 故意跨轮重复，检验运行时的 ID 改写
			if w.chance(0.3) {
				id = ""
			}
			resp.ToolCalls = append(resp.ToolCalls, &v1.ToolCall{CallId: id, Capability: name, ArgumentsJson: `{"n":1}`})
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
