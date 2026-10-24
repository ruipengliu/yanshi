package eval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/askuser"
	"yanshi/internal/call"
	"yanshi/internal/card"
	"yanshi/internal/clock"
	"yanshi/internal/delegation"
	"yanshi/internal/eventlog"
	"yanshi/internal/feed"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/mcpcap"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/realtime"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/xinlimock"
)

// BusinessLine 是评测使用的业务线；NoHealthBusinessLine 是未开启健康信息的业务线（setup.no_health）。
const (
	BusinessLine         = "eval"
	NoHealthBusinessLine = "eval-nohealth"
)

type Config struct {
	// Agents 是基础 AgentDef；用例的覆盖项与 ModelOverride 会派生出新版本。
	Agents *agentdef.Registry
	Model  *model.Gateway
	// Judge 是评分模型（"provider/model"）。
	Judge string
	// EmbedModel 非空时 Memory 按向量检索。
	EmbedModel string
	// ModelOverride 非空时替换被评测 AgentDef 的模型，用于在同一评测集上比较模型。
	ModelOverride string
	// AgentVersion 非空时评测该版本，而不是默认（稳定）版本：灰度前的评测门槛（docs/design/m4-agent-rollout.md §6）。
	AgentVersion string
	// Sandbox 为 nil 时跳过 requires: [sandbox] 的用例。
	Sandbox sandbox.Provider
	// Realtime 与 Speak（把文字合成为 16 kHz PCM 语音）都配置时才运行 Call 用例（call: true），否则跳过。
	Realtime *realtime.Gateway
	Speak    func(ctx context.Context, text string) ([]byte, error)
	// Trials > 0 时覆盖用例的运行次数。
	Trials int
	// Parallel 是同时运行的次数，默认 4。
	Parallel    int
	TurnTimeout time.Duration
	// LogDir 非空时，把每次运行的 Session 日志写成 <用例>-<序号>.jsonl，用于查看压缩摘要、调用顺序等过程
	// （评测数据是合成的，不含个人数据）。
	LogDir string
	// LeaseTTL 是 Worker 的租约时长，决定"杀掉" Worker 后多久被接管（默认 10 秒）。
	LeaseTTL time.Duration
	Logger   *slog.Logger
	// Progress 在每次运行结束时调用，用于显示进度。
	Progress func(caseName string, trial int, passed bool)
}

type instance struct {
	cfg     Config
	svc     *service.Service
	hub     *node.Hub
	mems    *memory.Service
	agents  *agentdef.Registry
	version map[string]string // 用例名 → 派生的 AgentDef 版本
	workers *workerPool
	arts    *artifact.Service
	crm     *crmServer
	xinli   *xinlimock.Server
	calls   *call.Manager

	// controllerDone 在沙箱控制器退出（或未启动）时关闭。
	controllerDone chan struct{}
}

// workerPool 管理 Worker，支持"杀掉"一个 Worker 并补充新的（CrashAfterCalls）。
type workerPool struct {
	mu      sync.Mutex
	ctx     context.Context
	workers map[string]*runtime.Worker
	next    int
	newCfg  func(id string) runtime.Config
}

func (p *workerPool) spawn() {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := fmt.Sprintf("eval-worker-%d", p.next)
	p.next++
	w := runtime.New(p.newCfg(id))
	p.workers[id] = w
	go w.Run(p.ctx)
}

// crash "杀掉" Worker（runtime.Worker.Kill）：进行中的模型调用或能力调用被中断，租约不释放，与进程崩溃相同。
// 随后补充一个新 Worker，保持并发度。
func (p *workerPool) crash(id string) bool {
	p.mu.Lock()
	w, ok := p.workers[id]
	delete(p.workers, id)
	p.mu.Unlock()
	if !ok {
		return false
	}
	w.Kill()
	p.spawn()
	return true
}

func (c *Config) defaults() {
	if c.Parallel <= 0 {
		c.Parallel = 4
	}
	if c.TurnTimeout <= 0 {
		c.TurnTimeout = 4 * time.Minute
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 10 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

// Run 运行用例集，返回报告。单个用例出错（而非断言失败）时记为失败并继续。
func Run(ctx context.Context, cfg Config, cases []*Case) (*Report, error) {
	cfg.defaults()
	// 评测实例（Worker、沙箱控制器）只活到本次运行结束。存储在内存中，进程退出后没有 Janitor 回收沙箱，
	// 所以结束时先停下实例，再销毁用过的全部沙箱（含委托的子 Session 的）。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var used *usedSandboxes
	if cfg.Sandbox != nil {
		used = &usedSandboxes{Provider: cfg.Sandbox, ids: map[string]bool{}}
		cfg.Sandbox = used
	}
	in, err := start(runCtx, cfg, cases)
	if err != nil {
		return nil, err
	}
	rep := &Report{StartedAt: time.Now().UTC(), Judge: cfg.Judge, ModelOverride: cfg.ModelOverride}
	results := make([]*CaseResult, len(cases))
	type job struct{ c, trial int }
	var jobs []job
	for i, c := range cases {
		res := &CaseResult{Name: c.Name, Failures: map[string]int{}}
		results[i] = res
		if slices.Contains(c.Requires, "sandbox") && cfg.Sandbox == nil {
			res.Skipped = "requires sandbox"
			continue
		}
		if c.Call && (cfg.Realtime == nil || cfg.Speak == nil) {
			res.Skipped = "requires a realtime model and speech synthesis"
			continue
		}
		res.Trials = c.Trials
		if cfg.Trials > 0 {
			res.Trials = cfg.Trials
		}
		for t := range res.Trials {
			jobs = append(jobs, job{i, t})
		}
	}
	var mu sync.Mutex
	sem := make(chan struct{}, cfg.Parallel)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			tr := in.trial(ctx, cases[j.c], j.trial)
			mu.Lock()
			results[j.c].add(tr)
			mu.Unlock()
			if cfg.Progress != nil {
				cfg.Progress(cases[j.c].Name, j.trial, tr.Passed)
			}
		}()
	}
	wg.Wait()
	for _, r := range results {
		r.finish()
		rep.Cases = append(rep.Cases, *r)
	}
	cancel()
	if used != nil {
		<-in.controllerDone // 控制器退出后不会再创建沙箱
		rep.LeakedSandboxes = used.destroyAll(context.WithoutCancel(ctx), cfg.Logger)
	}
	return rep, ctx.Err()
}

// usedSandboxes 记下本次评测创建过的沙箱，供结束时销毁。
type usedSandboxes struct {
	sandbox.Provider
	mu  sync.Mutex
	ids map[string]bool
}

func (p *usedSandboxes) Ensure(ctx context.Context, id string) error {
	// 先记下再创建：创建到一半失败也可能留下容器或工作区。
	p.mu.Lock()
	p.ids[id] = true
	p.mu.Unlock()
	return p.Provider.Ensure(ctx, id)
}

// destroyAll 销毁记下的全部沙箱，返回销毁失败的 ID。
func (p *usedSandboxes) destroyAll(ctx context.Context, logger *slog.Logger) []string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	p.mu.Lock()
	ids := slices.Sorted(maps.Keys(p.ids))
	p.mu.Unlock()
	var failed []string
	for _, id := range ids {
		if err := p.Provider.Destroy(ctx, id); err != nil {
			logger.Warn("eval: destroy sandbox", "sandbox", id, "err", err)
			failed = append(failed, id)
		}
	}
	return failed
}

func start(ctx context.Context, cfg Config, cases []*Case) (*instance, error) {
	clk := clock.Real{}
	st := app.Memory(clk)
	agents, versions, err := derive(cfg, cases)
	if err != nil {
		return nil, err
	}
	opts := app.Options{
		Clock: clk, Agents: agents, Model: cfg.Model, Blobs: artifact.NewMemBlobs(), NodeAuth: node.InsecureDevAuth{},
		SandboxTools: cfg.Sandbox != nil, DefaultTimeout: 10 * time.Minute,
		// 评测业务线视为已取得健康信息的单独同意，以评测模型对健康信息的处理（ADR-0022）。
		HealthAllowed: func(bl string) bool { return bl == BusinessLine },
		Live:          live.NewMemBus(), LeaseTTL: cfg.LeaseTTL, Logger: cfg.Logger,
	}
	if cfg.EmbedModel != "" {
		opts.Embedder, opts.EmbedModel = cfg.Model, cfg.EmbedModel
	}
	core := app.NewCore(st, opts)
	idle := &workqueue.IdleGate{Ready: st.Queue.(workqueue.Signaler).Ready()}
	pool := &workerPool{ctx: ctx, workers: map[string]*runtime.Worker{}, newCfg: func(id string) runtime.Config {
		return core.WorkerConfig(id, workqueue.Pool{}, idle)
	}}
	for range 2 * cfg.Parallel {
		pool.spawn()
	}
	controllerDone := make(chan struct{})
	if cfg.Sandbox != nil {
		go func() {
			defer close(controllerDone)
			core.ControllerConfig("eval-sandbox", cfg.Sandbox, nil).Run(ctx)
		}()
	} else {
		close(controllerDone)
	}
	in := &instance{cfg: cfg, svc: core.Service, hub: core.Hub, mems: core.Memory, agents: agents, version: versions, workers: pool, arts: core.Artifacts,
		calls: &call.Manager{Service: core.Service, Realtime: cfg.Realtime, Feed: feed.Source{Log: st.Log, Live: opts.Live}, Logger: cfg.Logger}}
	in.controllerDone = controllerDone
	var servers []mcpcap.Server
	if slices.ContainsFunc(cases, func(c *Case) bool { return c.Setup.CRM }) {
		if in.crm, err = startCRM(ctx); err != nil {
			return nil, err
		}
		servers = append(servers, in.crm.server)
	}
	if slices.ContainsFunc(cases, func(c *Case) bool { return c.Setup.Xinli != nil }) {
		if in.xinli, err = xinlimock.Start(ctx, BusinessLine, NoHealthBusinessLine); err != nil {
			return nil, err
		}
		servers = append(servers, in.xinli.Servers...)
	}
	if len(servers) > 0 {
		core.Catalog.MCP = &mcpcap.Connector{Servers: servers, Artifacts: core.Artifacts, Log: st.Log, Logger: cfg.Logger}
	}
	return in, nil
}

// derive 为每个用例派生 AgentDef：应用用例的上下文覆盖与 ModelOverride，版本号带上用例名以免冲突。
func derive(cfg Config, cases []*Case) (*agentdef.Registry, map[string]string, error) {
	defs := cfg.Agents.All()
	versions := map[string]string{}
	for _, c := range cases {
		base, err := cfg.Agents.Default(c.Agent)
		if cfg.AgentVersion != "" {
			base, err = cfg.Agents.Get(&v1.AgentRef{Name: c.Agent, Version: cfg.AgentVersion})
		}
		if err != nil {
			return nil, nil, fmt.Errorf("case %s: %w", c.Name, err)
		}
		d := *base
		d.Version = base.Version + "+eval-" + c.Name
		d.Capabilities = append(slices.Clone(base.Capabilities), c.Capabilities...)
		if cfg.ModelOverride != "" {
			d.Model = cfg.ModelOverride
		}
		if w := c.Context.Window; w > 0 {
			d.Context = agentdef.Context{Window: w, CompactAt: base.Context.CompactAt, KeepRecent: c.Context.KeepRecent,
				MaxToolResult: c.Context.MaxToolResult, SummaryModel: base.Context.SummaryModel}
		}
		defs = append(defs, &d)
		versions[c.Name] = d.Version
		for _, a := range c.Setup.Agents {
			defs = append(defs, &agentdef.Def{Name: a.Name, Version: "eval", Description: a.Description, Instructions: a.Instructions,
				Model: d.Model, Capabilities: a.Capabilities, MaxTurns: base.MaxTurns, Context: base.Context})
		}
	}
	reg, err := agentdef.NewRegistry(defs...)
	return reg, versions, err
}

// TrialResult 是一个用例的一次运行。
type TrialResult struct {
	Passed     bool
	Assertions []Assertion
	JudgeScore []int
	Tokens     uint64
	Duration   time.Duration
	// 各轮合计的调用、上下文压缩与接管次数。
	Calls, Compactions, Takeovers int
	// InputTokens 与 CachedInputTokens 用于计算前缀缓存命中率。
	InputTokens, CachedInputTokens uint64
	Err                            string
}

type Assertion struct {
	Turn   int
	Name   string
	Pass   bool
	Detail string
	// Infra 表示失败源于评测设施（如评分模型不可用），而不是被评测的 Agent。
	Infra bool
}

func (in *instance) trial(ctx context.Context, c *Case, trial int) (tr TrialResult) {
	start := time.Now()
	defer func() {
		tr.Duration = time.Since(start)
		tr.Passed = tr.Err == ""
		for _, a := range tr.Assertions {
			tr.Passed = tr.Passed && a.Pass
		}
	}()
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	endUser := fmt.Sprintf("eval-%s-%d-%s", c.Name, trial, ids.Random()()[:6])
	bl := BusinessLine
	if c.Setup.NoHealth {
		bl = NoHealthBusinessLine
	}
	if c.Setup.Xinli != nil {
		x := c.Setup.Xinli
		filed, exchanges := slices.Clone(x.Filed), slices.Clone(x.Exchanges)
		for i := range filed {
			filed[i].Date = relativeDate(filed[i].Date)
		}
		for i := range exchanges {
			exchanges[i].Date = relativeDate(exchanges[i].Date)
		}
		in.xinli.Seed(bl, endUser, xinlimock.SeedData{Projects: x.Projects, Days: x.Days, Commitments: x.Commitments, Filed: filed, Exchanges: exchanges})
	}
	// 预置记忆的 ID 与生产一致不可猜（由随机调用 ID 派生）：顺序 ID 会让模型按规律"猜出"未见过的 ID，
	// 替换掉另一条记忆（memory-update-crowded 中观察到）。
	prevMemory := ""
	for _, m := range c.Setup.Memories {
		var at time.Time
		if m.Recorded != "" {
			t, err := time.Parse(time.DateOnly, m.Recorded)
			if err != nil {
				tr.Err = "setup memory: " + err.Error()
				return tr
			}
			at = t
		}
		req := memory.SaveRequest{BusinessLine: bl, EndUser: endUser, CallID: "setup_" + ids.Random()(),
			Category: memory.Category(m.Category), Content: m.Content, At: at, ValidUntil: m.ValidUntil}
		if m.ReplacesPrevious {
			req.Replaces = prevMemory
		}
		if m.ExpiredOn != "" {
			// 已过期的不能经 Save 写入（过了有效期的事实不必记住），直接写入存储。
			d, err := time.ParseInLocation(time.DateOnly, m.ExpiredOn, captureZone)
			if err != nil {
				tr.Err = "setup memory: " + err.Error()
				return tr
			}
			mem := &memory.Memory{ID: "mem_" + req.CallID, BusinessLine: bl, EndUser: endUser, Category: req.Category, Content: m.Content,
				SourceCall: req.CallID, CreatedAt: at, UpdatedAt: at, ValidUntil: d.AddDate(0, 0, 1)}
			if err := in.mems.Store.Put(tctx, mem, nil); err != nil {
				tr.Err = "setup memory: " + err.Error()
				return tr
			}
			prevMemory = mem.ID
			continue
		}
		saved, err := in.mems.Save(tctx, req)
		if err != nil {
			tr.Err = "setup memory: " + err.Error()
			return tr
		}
		prevMemory = saved.ID
	}
	var devs []*device
	setups := c.Setup.Devices
	if s := c.Setup.Device; s != nil {
		setups = append([]DeviceSetup{*s}, setups...)
	}
	for i, s := range setups {
		dev := &device{kind: s.Kind, files: map[string]string{}, writes: map[string]string{}, arts: in.arts}
		for k, v := range s.Files {
			dev.files[k] = v
		}
		label := s.Label
		if label == "" {
			label = "macbook"
		}
		nodeID := "node-" + endUser
		if i > 0 {
			nodeID += "-" + label
		}
		if err := dev.run(tctx, in.hub, nodeID, bl, endUser, label, s.OnlineAfter); err != nil {
			tr.Err = "setup device: " + err.Error()
			return tr
		}
		devs = append(devs, dev)
	}
	var sid string
	var sessions []string
	// topics 是每个 Session 按提交顺序的旁听主题（CaptureSegment.Topic），用于计算项目归集的纯度与碎片率。
	topics := map[string][]string{}
	if in.cfg.LogDir != "" {
		defer func() { in.dumpLogs(ctx, fmt.Sprintf("%s-%d", c.Name, trial+1), sessions) }()
	}
	var voice *call.Call
	var rec *callRecorder
	defer func() {
		if voice != nil {
			voice.End(service.CallEndHangup)
			<-voice.Done() // 结束记录写入之后再导出日志
		}
	}()
	for ti, turn := range c.Turns {
		if sid == "" || turn.NewSession {
			if c.Call && sid != "" {
				tr.Err = "call cases do not support new_session"
				return tr
			}
			id, err := in.svc.Create(tctx, service.CreateRequest{BusinessLine: bl, EndUser: endUser, Agent: c.Agent, AgentVersion: in.version[c.Name]})
			if err != nil {
				tr.Err = "create session: " + err.Error()
				return tr
			}
			sid = id
			sessions = append(sessions, id)
			if x := c.Setup.Xinli; x != nil && x.ProjectSession != "" {
				in.xinli.Bind(bl, endUser, x.ProjectSession, id)
			}
			if c.Call {
				if voice, rec, err = in.startCall(tctx, sid); err != nil {
					tr.Err = "start call: " + err.Error()
					return tr
				}
			}
		}
		for _, g := range turn.Captures {
			topics[sid] = append(topics[sid], g.Topic)
		}
		if err := in.submitCaptures(tctx, sid, turn.Captures); err != nil {
			tr.Err = fmt.Sprintf("turn %d: captures: %v", ti+1, err)
			return tr
		}
		var obs *observation
		var err error
		if c.Call {
			obs, err = in.callTurn(tctx, voice, rec, sid, turn, c.Timeout)
		} else {
			obs, err = in.turn(tctx, sid, turn, c.Timeout)
		}
		if err != nil {
			tr.Err = fmt.Sprintf("turn %d: %v", ti+1, err)
			return tr
		}
		tr.Tokens += obs.tokens
		tr.Calls, tr.Compactions, tr.Takeovers = tr.Calls+len(obs.calls), tr.Compactions+obs.compactions, tr.Takeovers+obs.takeovers
		tr.InputTokens, tr.CachedInputTokens = tr.InputTokens+obs.inTokens, tr.CachedInputTokens+obs.cachedTokens
		obs.writes = map[string]string{}
		for _, dev := range devs {
			w, sent := dev.snapshot()
			maps.Copy(obs.writes, w)
			obs.sent += sent
		}
		obs.tickets = in.crm.ticketsOf(endUser)
		if in.xinli != nil {
			x := in.xinli.State(bl, endUser)
			obs.xinli = &x
			if turn.Expect.Xinli != nil && turn.Expect.Xinli.Grouping != nil {
				if obs.topics, err = in.exchangeTopics(tctx, topics); err != nil {
					tr.Err = fmt.Sprintf("turn %d: %v", ti+1, err)
					return tr
				}
			}
		}
		// 断言只看有效的 Memory：被取代的作为历史保留（m4-memory-grant.md §11），不算"还记着"。
		obs.memories, _ = in.mems.Store.List(tctx, endUser, []memory.Scope{{BusinessLine: bl}}, time.Now(), memory.DefaultMaxPerUser)
		as, score := in.check(tctx, ti+1, turn, obs)
		tr.Assertions = append(tr.Assertions, as...)
		if score > 0 {
			tr.JudgeScore = append(tr.JudgeScore, score)
		}
	}
	return tr
}

// observation 是一轮中观察到的事实，断言据此判定。
type observation struct {
	input     string
	status    string
	calls     []string
	approvals int
	questions int
	reply     string
	tokens    uint64
	steps     []string // 给评分模型看的调用摘要
	writes    map[string]string
	sent      int
	memories  []*memory.Memory
	compacted bool // 到本轮结束时 Session 是否发生过上下文压缩
	// compactions 是本轮的上下文压缩次数，takeovers 是本轮 Run 的接管次数。
	compactions, takeovers int
	tickets                []string          // CRM 中本 EndUser 的工单
	xinli                  *xinlimock.State  // xinli MCP Server 中本 EndUser 的数据
	topics                 map[string]string // 旁听 Session 与引用编号（"ses/57.3"）→ 那段旁听的主题
	inTokens, cachedTokens uint64            // 本轮模型调用的输入 token 与其中命中前缀缓存的部分
	callArgs               []string          // 与 calls 一一对应的调用参数
	tasks                  []string          // Call 中派生的任务（run_task 的 task）
}

// turn 提交一轮输入，按 approve 自动作出审批决定，等待 Run 结束并收集本轮的事件。
func (in *instance) turn(ctx context.Context, sid string, turn Turn, timeout time.Duration) (*observation, error) {
	st, err := in.svc.Load(ctx, sid)
	if err != nil {
		return nil, err
	}
	after := st.Seq
	if timeout <= 0 {
		timeout = in.cfg.TurnTimeout
	}
	deadline := time.Now().Add(timeout)
	var runID string
	if turn.AwaitBackground {
		runID, err = in.awaitBackground(ctx, sid, after, deadline)
	} else {
		runID, err = in.submitTurn(ctx, sid, turn)
	}
	if err != nil {
		return nil, err
	}
	approve := turn.Approve == nil || *turn.Approve
	crashed := false
	asked := false
	answered := map[string]bool{}
	var run *session.Run
	var compacted bool
	for {
		st, err := in.svc.Load(ctx, sid)
		if err != nil {
			return nil, err
		}
		run = st.Run(runID)
		if run != nil && run.Status.Terminal() {
			compacted = st.Compaction != nil
			break
		}
		if run != nil {
			// ask_user 提问：按脚本回答；没有脚本时本轮停在提问处。
			if c := pendingQuestion(run, answered); c != nil {
				if turn.Answer == nil {
					compacted, asked = st.Compaction != nil, true
					break
				}
				answered[c.Call.GetCallId()] = true
				if err := in.answer(ctx, sid, c, turn.Answer); err != nil {
					return nil, err
				}
			}
		}
		if run != nil && turn.CrashAfterCalls > 0 && !crashed && len(run.Calls) >= turn.CrashAfterCalls && run.Status == session.RunRunning {
			if err := in.crashHolder(ctx, sid, run); err != nil {
				return nil, err
			}
			crashed = true
		}
		if run != nil {
			for _, c := range run.Calls {
				if c.AwaitingApproval() {
					if err := in.svc.Decide(ctx, sid, c.Call.GetCallId(), approve, "eval"); err != nil && !errors.Is(err, service.ErrConflict) {
						return nil, err
					}
				}
				// 委托出去的子 Run 的审批与提问同样路由到用户本人（docs/design/m5-multi-agent.md §2）：照本轮的脚本处理。
				if c.Dispatched() && delegation.IsNode(c.NodeID) {
					if err := in.attendChild(ctx, delegation.ChildSession(c.NodeID), turn, approve, answered); err != nil {
						return nil, err
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("run %s did not finish within %s", runID, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, after)
	if err != nil {
		return nil, err
	}
	input := turn.Input
	if turn.AwaitBackground {
		input = "（用户没有输入：设备上线后，之前转入后台的调用的结果由平台送达）"
	}
	obs := &observation{input: input, status: run.Status.String(), compacted: compacted, takeovers: run.Takeovers}
	if asked {
		obs.status = "asked"
	}
	collect(events, obs)
	// 子 Session 中发生的事计入本轮（审批、给评分模型看的步骤）；子 Run 的调用不计入本轮的调用，
	// 父 Run 只看到一次 agent__ 调用。
	for _, e := range events {
		if id := e.GetToolCallStarted().GetNodeId(); delegation.IsNode(id) {
			childEvents, err := eventlog.ReadAll(ctx, in.svc.Store.Log, delegation.ChildSession(id), 0)
			if err != nil {
				return nil, err
			}
			child := &observation{}
			collect(childEvents, child)
			obs.approvals += child.approvals
			for _, s := range child.steps {
				obs.steps = append(obs.steps, "（子 Agent）"+s)
			}
			if child.reply != "" {
				obs.steps = append(obs.steps, "（子 Agent）回复 "+clip(child.reply, judgeStepBytes))
			}
		}
	}
	return obs, nil
}

// submitTurn 提交本轮的输入（文字、界面上下文或卡片动作），返回开启或并入的 Run。
func (in *instance) submitTurn(ctx context.Context, sid string, turn Turn) (string, error) {
	var err error
	input := model.TextBlocks(turn.Input)
	if u := turn.UIContext; u != nil {
		input = append([]*v1.ContentBlock{{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{
			Screen: u.Screen, Ref: u.Ref, Selection: u.Selection, Content: u.Content}}}}, input...)
	}
	if turn.CardAction != nil {
		if input, err = in.cardAction(ctx, sid, turn.CardAction); err != nil {
			return "", err
		}
	}
	if turn.FromDevice != "" {
		input = service.WithDevice(input, turn.FromDevice, true)
	}
	res, err := in.svc.Submit(ctx, sid, input)
	if err != nil {
		return "", err
	}
	return res.RunID, nil
}

// checkXinli 判定对 xinli 业务线数据的断言（XinliExpect）。
func (in *instance) checkXinli(add func(string, bool, string), e *XinliExpect, st *xinlimock.State) {
	if st == nil {
		add("xinli", false, "the case has no setup.xinli")
		return
	}
	inRange := func(r *Range, n int) bool {
		return r == nil || ((r.Min == nil || n >= *r.Min) && (r.Max == nil || n <= *r.Max))
	}
	pending := 0
	byProject := map[string]int{}
	for i := range st.Exchanges {
		if st.Exchanges[i].Pending() {
			pending++
		}
		byProject[st.Exchanges[i].Project]++
	}
	add("xinli exchanges", inRange(e.Exchanges, len(st.Exchanges)), fmt.Sprintf("%d exchanges", len(st.Exchanges)))
	add("xinli pending", inRange(e.Pending, pending), fmt.Sprintf("%d pending", pending))
	add("xinli projects", inRange(e.Projects, len(st.Projects)), fmt.Sprintf("%d projects", len(st.Projects)))
	for _, id := range slices.Sorted(maps.Keys(e.InProject)) {
		r := e.InProject[id]
		add("xinli in_project "+id, inRange(&r, byProject[id]), fmt.Sprintf("%d exchanges in %s", byProject[id], id))
	}
	var all []string
	for _, m := range st.Commitments {
		all = append(all, fmt.Sprintf("[%s %s %s %s] %s %s", m.Kind, m.Status, m.DueDate, m.Project, m.Content, m.Unverified))
	}
	detail := "commitments: " + clip(strings.Join(all, "; "), 400)
	for _, want := range e.Commitments {
		add("xinli commitment "+want.Kind+" "+want.Contains, slices.ContainsFunc(st.Commitments, func(m xinlimock.Commitment) bool { return commitmentMatches(m, want) }), detail)
	}
	for _, want := range e.NoCommitments {
		add("xinli no_commitment "+want.Kind+" "+want.Contains, !slices.ContainsFunc(st.Commitments, func(m xinlimock.Commitment) bool { return commitmentMatches(m, want) }), detail)
	}
	proposed := func(p [2]string) bool {
		return slices.ContainsFunc(st.Merges, func(m xinlimock.Merge) bool {
			return (m.Into == p[0] && m.From == p[1]) || (m.Into == p[1] && m.From == p[0])
		})
	}
	merges := fmt.Sprintf("merges %+v", st.Merges)
	for _, p := range e.Merges {
		add("xinli merge "+p[0]+" "+p[1], proposed(p), merges)
	}
	for _, p := range e.NoMerges {
		add("xinli no_merge "+p[0]+" "+p[1], !proposed(p), merges)
	}
	var habits []string
	for _, w := range st.Weeks {
		for _, h := range w.Habits {
			habits = append(habits, h.Habit)
		}
	}
	hasHabit := func(sub string) bool {
		return slices.ContainsFunc(habits, func(h string) bool { return strings.Contains(h, sub) })
	}
	for _, sub := range e.WeekHabits {
		add("xinli week_habit "+sub, hasHabit(sub), fmt.Sprintf("habits %q", habits))
	}
	for _, sub := range e.NoWeekHabits {
		add("xinli no_week_habit "+sub, !hasHabit(sub), fmt.Sprintf("habits %q", habits))
	}
	for _, id := range slices.Sorted(maps.Keys(e.Status)) {
		want, status := e.Status[id], ""
		for _, p := range st.Projects {
			if p.ID == id {
				status = p.Status + " " + strings.Join(p.NextSteps, " ") + " " + strings.Join(p.OpenQuestions, " ")
			}
		}
		for _, sub := range want.Contains {
			add("xinli status "+id+" contains "+sub, strings.Contains(status, sub), "status: "+clip(status, 300))
		}
		for _, sub := range want.Excludes {
			add("xinli status "+id+" excludes "+sub, !strings.Contains(status, sub), "status: "+clip(status, 300))
		}
	}
}

// relativeDate 把 "today-N" 换成运行当天之前第 N 天的日期（Asia/Shanghai），其余原样返回。
func relativeDate(s string) string {
	rest, ok := strings.CutPrefix(s, "today")
	if !ok {
		return s
	}
	n := 0
	if rest != "" {
		if _, err := fmt.Sscanf(rest, "-%d", &n); err != nil {
			return s
		}
	}
	return time.Now().In(captureZone).AddDate(0, 0, -n).Format(time.DateOnly)
}

// exchangeTopics 按日志顺序把每段旁听对应到提交时给出的主题：键是"Session/事件序号.段序号"。
func (in *instance) exchangeTopics(ctx context.Context, topics map[string][]string) (map[string]string, error) {
	out := map[string]string{}
	for sid, list := range topics {
		events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, 0)
		if err != nil {
			return nil, err
		}
		i := 0
		for _, e := range events {
			for j := range e.GetCaptureAppended().GetSegments() {
				if i < len(list) {
					out[fmt.Sprintf("%s/%d.%d", sid, e.GetSeq(), j)] = list[i]
				}
				i++
			}
		}
	}
	return out, nil
}

// checkGrouping 计算项目归集的碎片率与纯度（captures-weekly-review.md §6）。每段交出的交谈取它第一段旁听的主题，没有
// 主题的（闲聊）不计入碎片率。碎片率：每个主题的交谈分散在几个项目中（没有归入项目的每段各算一个），按主题平均；
// 纯度：每个项目中占多数的主题所占的比例（闲聊算作杂质），按交谈数加权。
func checkGrouping(add func(string, bool, string), want *GroupingExpect, st *xinlimock.State, topics map[string]string) {
	byTopic := map[string]map[string]bool{}
	byProject := map[string]map[string]int{}
	for i := range st.Exchanges {
		e := &st.Exchanges[i]
		topic := topics[e.Session+"/"+strings.TrimPrefix(e.ID, "x")]
		if topic != "" {
			if byTopic[topic] == nil {
				byTopic[topic] = map[string]bool{}
			}
			where := e.Project
			if where == "" {
				where = "unfiled:" + e.ID + "@" + e.Session
			}
			byTopic[topic][where] = true
		}
		if e.Project != "" {
			if byProject[e.Project] == nil {
				byProject[e.Project] = map[string]int{}
			}
			if topic == "" {
				topic = "(闲聊)"
			}
			byProject[e.Project][topic]++
		}
	}
	frag := 0.0
	for _, ps := range byTopic {
		frag += float64(len(ps))
	}
	if len(byTopic) > 0 {
		frag /= float64(len(byTopic))
	}
	total, majority := 0, 0
	for _, ts := range byProject {
		best := 0
		for t, n := range ts {
			total += n
			if n > best && t != "(闲聊)" {
				best = n
			}
		}
		majority += best
	}
	purity := 1.0
	if total > 0 {
		purity = float64(majority) / float64(total)
	}
	detail := fmt.Sprintf("fragmentation %.2f over %d topics, purity %.2f over %d exchanges in projects; topics → projects %v", frag, len(byTopic), purity, total, byTopic)
	if r := want.Fragmentation; r != nil {
		add("xinli fragmentation", (r.Min == nil || frag >= *r.Min) && (r.Max == nil || frag <= *r.Max), detail)
	}
	if want.Purity != nil {
		add("xinli purity", purity >= *want.Purity, detail)
	}
	if len(byTopic) < want.Topics {
		add("xinli topics", false, detail)
	}
}

func commitmentMatches(m xinlimock.Commitment, want CommitmentMatch) bool {
	return (want.Kind == "" || m.Kind == want.Kind) && strings.Contains(m.Content, want.Contains) &&
		(want.DueDate == "" || m.DueDate == want.DueDate) && (want.Status == "" || m.Status == want.Status) &&
		(want.Project == "" || m.Project == want.Project) && (want.Verified == nil || *want.Verified == (m.Unverified == ""))
}

// captureZone 是评测中 HostApp 所在的时区：用例的 at 按它解读。
var captureZone, _ = time.LoadLocation("Asia/Shanghai")

// submitCaptures 以 HostApp 的身份把一批旁听提交到 Session（docs/design/captures.md §3）。
func (in *instance) submitCaptures(ctx context.Context, sid string, segs []CaptureSegment) error {
	if len(segs) == 0 {
		return nil
	}
	out := make([]*v1.CaptureSegment, 0, len(segs))
	for _, g := range segs {
		seg := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: g.Text}
		if g.At != "" {
			at, err := time.ParseInLocation(time.DateTime, g.At+":00", captureZone)
			if err != nil {
				return fmt.Errorf("capture at %q: %w", g.At, err)
			}
			seg.SpokenAt, seg.TimeZone = timestamppb.New(at), captureZone.String()
		}
		if g.Speaker == "other" {
			seg.Speaker, seg.SpeakerKey = v1.Speaker_SPEAKER_OTHER, g.Key
			if seg.SpeakerKey == "" {
				seg.SpeakerKey = "s1"
			}
		}
		out = append(out, seg)
	}
	// 长的一天按批提交，与 HostApp 每分钟提交一批相同；一批至多 session.MaxCaptureSegments 段。
	for len(out) > 0 {
		n := min(len(out), 100)
		if _, err := in.svc.SubmitCaptures(ctx, sid, "cap_"+ids.Random()(), out[:n]); err != nil {
			return err
		}
		out = out[n:]
	}
	return nil
}

// awaitBackground 等待 seq after 之后的后台调用了结（ADR-0035），返回它开启或并入的 Run。
func (in *instance) awaitBackground(ctx context.Context, sid string, after uint64, deadline time.Time) (string, error) {
	for {
		events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, after)
		if err != nil {
			return "", err
		}
		for _, e := range events {
			if session.BackgroundInput(e.GetRunRequested().GetInput()) != nil {
				return e.GetRunRequested().GetRunId(), nil
			}
			if session.BackgroundInput(e.GetSteered().GetInput()) != nil {
				return e.GetSteered().GetRunId(), nil
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("no background result arrived in time")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// attendChild 处理子 Session 中进行中 Run 的审批与提问，与父 Run 相同。
func (in *instance) attendChild(ctx context.Context, child string, turn Turn, approve bool, answered map[string]bool) error {
	st, err := in.svc.Load(ctx, child)
	if errors.Is(err, service.ErrNotFound) {
		return nil // 尚未创建
	}
	if err != nil {
		return err
	}
	a := st.Active()
	if a == nil {
		return nil
	}
	for _, c := range a.Calls {
		if c.AwaitingApproval() {
			if err := in.svc.Decide(ctx, child, c.Call.GetCallId(), approve, "eval"); err != nil && !errors.Is(err, service.ErrConflict) {
				return err
			}
		}
	}
	if c := pendingQuestion(a, answered); c != nil && turn.Answer != nil {
		answered[c.Call.GetCallId()] = true
		return in.answer(ctx, child, c, turn.Answer)
	}
	return nil
}

// collect 从本轮的事件中收集调用、审批、回复与用量。
func collect(events []*v1.Event, obs *observation) {
	results := map[string]string{}
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *v1.Event_AssistantMessage:
			m := p.AssistantMessage
			obs.tokens += m.GetUsage().GetInputTokens() + m.GetUsage().GetOutputTokens()
			obs.inTokens, obs.cachedTokens = obs.inTokens+m.GetUsage().GetInputTokens(), obs.cachedTokens+m.GetUsage().GetCachedInputTokens()
			for _, tc := range m.GetToolCalls() {
				if tc.GetCapability() == askuser.Capability {
					obs.questions++
					// 提问也是给用户看的回复：评分模型据此判断是否问得恰当。
					if q, err := askuser.Parse(tc.GetArgumentsJson()); err == nil {
						if obs.reply != "" {
							obs.reply += "\n\n"
						}
						obs.reply += describeQuestion(q)
					}
				}
				obs.calls = append(obs.calls, tc.GetCapability())
				obs.callArgs = append(obs.callArgs, tc.GetArgumentsJson())
				obs.steps = append(obs.steps, fmt.Sprintf("调用 %s %s", tc.GetCapability(), clip(tc.GetArgumentsJson(), judgeStepBytes)))
			}
			// 回复是本轮全部助手文本：模型常把结论写在带调用的消息里（如同时保存 Memory），
			// 之后只补一句话；用户看到的是全部文本。
			if t := model.Text(m.GetContent()); t != "" {
				if obs.reply != "" {
					obs.reply += "\n\n"
				}
				obs.reply += t
			}
		case *v1.Event_ToolResult:
			r := p.ToolResult
			prefix := "结果"
			if r.GetIsError() {
				prefix = "错误"
			}
			results[r.GetCallId()] = prefix
			obs.steps = append(obs.steps, fmt.Sprintf("%s %s", prefix, clip(model.Text(r.GetContent()), judgeStepBytes)))
		case *v1.Event_ApprovalRequested:
			obs.approvals++
			obs.steps = append(obs.steps, "请求审批："+clip(p.ApprovalRequested.GetSummary(), 120))
		case *v1.Event_ApprovalDecided:
			obs.steps = append(obs.steps, fmt.Sprintf("用户审批：%v", p.ApprovalDecided.GetApproved()))
		case *v1.Event_ContextCompacted:
			obs.compactions++
			u := p.ContextCompacted.GetUsage()
			obs.tokens += u.GetInputTokens() + u.GetOutputTokens()
			obs.inTokens, obs.cachedTokens = obs.inTokens+u.GetInputTokens(), obs.cachedTokens+u.GetCachedInputTokens()
		}
	}
}

// pendingQuestion 返回 Run 中尚未回答、也未经本轮脚本回答过的 ask_user 提问。
func pendingQuestion(run *session.Run, answered map[string]bool) *session.Call {
	for _, c := range run.Calls {
		if c.Dispatched() && c.NodeID == askuser.NodeID && !answered[c.Call.GetCallId()] {
			return c
		}
	}
	return nil
}

// answer 按脚本回答一次提问。
func (in *instance) answer(ctx context.Context, sid string, c *session.Call, a *AnswerScript) error {
	if a.Reply != "" {
		_, err := in.svc.Submit(ctx, sid, model.TextBlocks(a.Reply))
		return err
	}
	q, err := askuser.Parse(c.Call.GetArgumentsJson())
	if err != nil {
		return err
	}
	ans := &askuser.Answer{Values: a.Values, Text: a.Text}
	if a.Choose != "" {
		for _, o := range q.Options {
			if strings.Contains(o.Label, a.Choose) || strings.Contains(o.ID, a.Choose) {
				ans.Selected = append(ans.Selected, o.ID)
				break
			}
		}
		if len(ans.Selected) == 0 && ans.Text == "" {
			// 没有匹配的选项：用户只能自己打字说明。
			ans.Text = a.Choose
		}
	}
	err = in.svc.Answer(ctx, sid, c.Call.GetCallId(), ans)
	if errors.Is(err, service.ErrInvalid) && ans.Text == "" {
		// 例如缺了必填字段：改为打字回答，与真实用户在表单不合适时的做法一致。
		ans = &askuser.Answer{Text: a.Choose}
		err = in.svc.Answer(ctx, sid, c.Call.GetCallId(), ans)
	}
	if errors.Is(err, service.ErrConflict) {
		return nil
	}
	return err
}

func describeQuestion(q *askuser.Question) string {
	var b strings.Builder
	b.WriteString("（提问）" + q.Question)
	for _, o := range q.Options {
		b.WriteString("\n- " + o.Label)
	}
	for _, f := range q.Fields {
		b.WriteString("\n- 填写：" + f.Label)
	}
	return b.String()
}

// judgeStepBytes 是给评分模型看的每个调用参数与结果的上限。太短时评分模型看不到文件的后半部分，
// 会把回复中来自那里的内容判为"编造"（scenario-a-minutes 中观察到：200 字节时把会议记录的末尾判为编造）。
const judgeStepBytes = 3000

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func matches(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

func memoryMatches(m *memory.Memory, want MemoryMatch) bool {
	if want.HasValidUntil && m.ValidUntil.IsZero() {
		return false
	}
	category := want.Category == "" || string(m.Category) == want.Category
	if not, ok := strings.CutPrefix(want.Category, "!"); ok {
		category = string(m.Category) != not
	}
	return category && strings.Contains(m.Content, want.Contains) &&
		(want.Without == "" || !strings.Contains(m.Content, want.Without))
}

// check 判定一轮的断言；有评分标准时请评分模型打分，返回分数（0 表示没有评分）。
func (in *instance) check(ctx context.Context, turn int, t Turn, o *observation) ([]Assertion, int) {
	var out []Assertion
	add := func(name string, pass bool, detail string) {
		out = append(out, Assertion{Turn: turn, Name: name, Pass: pass, Detail: detail})
	}
	e := t.Expect
	want := e.Status
	if want == "" {
		want = "completed"
	}
	add("status", slices.Contains(strings.Split(want, "|"), o.status), fmt.Sprintf("status %s, want %s", o.status, want))
	for _, g := range e.Calls {
		add("calls "+g, slices.ContainsFunc(o.calls, func(c string) bool { return matches([]string{g}, c) }), fmt.Sprintf("calls %v", o.calls))
	}
	for g, max := range e.MaxCalls {
		n := 0
		for _, c := range o.calls {
			if matches([]string{g}, c) {
				n++
			}
		}
		add("max_calls "+g, n <= max, fmt.Sprintf("%d calls matching %s, at most %d", n, g, max))
	}
	for _, g := range e.NoCalls {
		add("no_calls "+g, !slices.ContainsFunc(o.calls, func(c string) bool { return matches([]string{g}, c) }), fmt.Sprintf("calls %v", o.calls))
	}
	if r := e.Approvals; r != nil {
		ok := (r.Min == nil || o.approvals >= *r.Min) && (r.Max == nil || o.approvals <= *r.Max)
		add("approvals", ok, fmt.Sprintf("approvals %d", o.approvals))
	}
	if r := e.Questions; r != nil {
		ok := (r.Min == nil || o.questions >= *r.Min) && (r.Max == nil || o.questions <= *r.Max)
		add("questions", ok, fmt.Sprintf("questions %d", o.questions))
	}
	for _, s := range e.ReplyExcludes {
		add("reply_excludes "+s, !strings.Contains(o.reply, s), "reply: "+clip(o.reply, 200))
	}
	for _, s := range e.ReplyContains {
		add("reply_contains "+s, strings.Contains(o.reply, s) || strings.Contains(plainNumbers(o.reply), s), "reply: "+clip(o.reply, 200))
	}
	if e.ReplyMaxChars > 0 {
		n := utf8.RuneCountInString(o.reply)
		add("reply_max_chars", n <= e.ReplyMaxChars, fmt.Sprintf("reply has %d chars", n))
	}
	for _, m := range e.Memories {
		n := 0
		for _, x := range o.memories {
			if memoryMatches(x, m) {
				n++
			}
		}
		add(fmt.Sprintf("memory %s %q", m.Category, m.Contains), n > 0 && (m.Max == 0 || n <= m.Max),
			fmt.Sprintf("%d of %d memories match", n, len(o.memories)))
	}
	for _, m := range e.NoMemories {
		ok := !slices.ContainsFunc(o.memories, func(x *memory.Memory) bool { return memoryMatches(x, m) })
		add(fmt.Sprintf("no_memory %s %q", m.Category, m.Contains), ok, fmt.Sprintf("%d memories", len(o.memories)))
	}
	for p, sub := range e.DeviceWrites {
		got, ok := o.writes[p]
		add("device_writes "+p, ok && strings.Contains(got, sub), "written: "+clip(got, 120))
	}
	if e.Compacted {
		add("compacted", o.compacted, "session never compacted")
	}
	inRange := func(name string, r *Range, n int) {
		if r != nil {
			add(name, (r.Min == nil || n >= *r.Min) && (r.Max == nil || n <= *r.Max), fmt.Sprintf("%s %d", name, n))
		}
	}
	for g, sub := range e.CallArgs {
		ok := false
		for i, c := range o.calls {
			if matches([]string{g}, c) && strings.Contains(o.callArgs[i], sub) {
				ok = true
			}
		}
		add("call_args "+g, ok, fmt.Sprintf("calls %v", o.calls))
	}
	for g, sub := range e.NoCallArgs {
		var hit []string
		for i, c := range o.calls {
			if matches([]string{g}, c) && strings.Contains(o.callArgs[i], sub) {
				hit = append(hit, clip(o.callArgs[i], 200))
			}
		}
		add("no_call_args "+g, len(hit) == 0, fmt.Sprintf("arguments containing %q: %v", sub, hit))
	}
	if x := e.Xinli; x != nil {
		in.checkXinli(add, x, o.xinli)
		if x.Grouping != nil && o.xinli != nil {
			checkGrouping(add, x.Grouping, o.xinli, o.topics)
		}
		if x.Covers != nil && o.xinli != nil {
			covered, detail, err := judgeCoverage(ctx, in.cfg.Model, in.cfg.Judge, x.Covers.Points, o.xinli)
			if err != nil {
				out = append(out, Assertion{Turn: turn, Name: "xinli covers", Detail: "judge error: " + err.Error(), Infra: true})
			} else {
				add("xinli covers", covered >= x.Covers.Min, fmt.Sprintf("coverage %.2f, want at least %.2f: %s", covered, x.Covers.Min, detail))
			}
		}
		if x.VerifiedRate != nil && o.xinli != nil {
			n, ok := 0, 0
			for _, m := range o.xinli.Commitments {
				if m.Exchange != "" {
					n++
					if m.Unverified == "" {
						ok++
					}
				}
			}
			rate := 1.0
			if n > 0 {
				rate = float64(ok) / float64(n)
			}
			add("xinli verified_rate", rate >= *x.VerifiedRate, fmt.Sprintf("%d of %d commitments verified", ok, n))
		}
	}
	for _, want := range e.Tickets {
		add("tickets "+want, slices.ContainsFunc(o.tickets, func(t string) bool { return strings.Contains(t, want) }), fmt.Sprintf("tickets %v", o.tickets))
	}
	inRange("tasks", e.Tasks, len(o.tasks))
	for _, sub := range e.TaskContains {
		add("task_contains "+sub, slices.ContainsFunc(o.tasks, func(t string) bool { return strings.Contains(t, sub) }), fmt.Sprintf("tasks %q", o.tasks))
	}
	inRange("compactions", e.Compactions, o.compactions)
	inRange("takeovers", e.Takeovers, o.takeovers)
	if e.DeviceSent != nil {
		add("device_sent", o.sent == *e.DeviceSent, fmt.Sprintf("sent %d, want %d", o.sent, *e.DeviceSent))
	}
	score := 0
	if e.Judge != "" {
		s, reason, err := judge(ctx, in.cfg.Model, in.cfg.Judge, e.Judge, o)
		if err != nil {
			out = append(out, Assertion{Turn: turn, Name: "judge", Detail: "judge error: " + err.Error(), Infra: true})
		} else {
			score = s
			add("judge", s >= PassScore, fmt.Sprintf("score %d: %s", s, reason))
		}
	}
	return out, score
}

// crashHolder 找到当前持有 run 的 Worker（最近一次 AttemptStarted）并"杀掉"它。
func (in *instance) crashHolder(ctx context.Context, sid string, run *session.Run) error {
	events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, 0)
	if err != nil {
		return err
	}
	var holder string
	for _, e := range events {
		if a := e.GetAttemptStarted(); a != nil && a.GetRunId() == run.ID {
			holder = a.GetWorkerId()
		}
	}
	if holder == "" || !in.workers.crash(holder) {
		return fmt.Errorf("no worker holds run %s", run.ID)
	}
	if in.cfg.Logger != nil {
		in.cfg.Logger.Info("eval: crashed worker", "worker", holder, "session", sid, "run", run.ID)
	}
	return nil
}

// dumpLogs 把 sessions 的日志依次写入 LogDir/<name>.jsonl，每行一个事件（protojson）。
func (in *instance) dumpLogs(ctx context.Context, name string, sessions []string) {
	var b strings.Builder
	for _, sid := range sessions {
		events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, 0)
		if err != nil {
			continue
		}
		for _, e := range events {
			j, err := protojson.Marshal(e)
			if err == nil {
				b.Write(j)
				b.WriteByte('\n')
			}
		}
	}
	_ = os.MkdirAll(in.cfg.LogDir, 0o755)
	_ = os.WriteFile(filepath.Join(in.cfg.LogDir, name+".jsonl"), []byte(b.String()), 0o644)
}

// digitGroup 匹配数字中的千分位分隔符：逗号、空格及各种窄空格（"277,050"、"277 050"）。
var digitGroup = regexp.MustCompile(`(\d)[,，\x{0020}\x{00a0}\x{2009}\x{202f}](\d{3})`)

// plainNumbers 去掉数字中的千分位分隔符（"277,050" → "277050"），使 reply_contains 不受数字格式影响。
func plainNumbers(s string) string {
	for {
		t := digitGroup.ReplaceAllString(s, "$1$2")
		if t == s {
			return s
		}
		s = t
	}
}

// cardAction 组装本轮的卡片动作：在 Session 最近展示的卡片（或指定 card_id 的最新一版）上选按钮或条目。
func (in *instance) cardAction(ctx context.Context, sid string, a *CardActionScript) ([]*v1.ContentBlock, error) {
	var found *v1.Card
	var seq uint64
	err := session.ScanLog(ctx, in.svc.Store.Log, sid, func(e *v1.Event) bool {
		if c := card.Find(e.GetToolResult().GetContent()); c != nil && (a.Card == "" || c.GetCardId() == a.Card) {
			found, seq = c, e.GetSeq()
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("card_action: no card %q was shown", a.Card)
	}
	action := a.Action
	for _, b := range found.GetButtons() {
		if action == "" && (a.Label == "" || strings.Contains(b.GetLabel(), a.Label)) {
			action = b.GetId()
		}
	}
	for _, e := range found.GetBody() {
		for _, it := range e.GetList().GetItems() {
			if action == "" && a.Label != "" && it.GetAction() != "" && strings.Contains(it.GetTitle(), a.Label) {
				action = it.GetAction()
			}
		}
	}
	if action == "" {
		return nil, fmt.Errorf("card_action: card %s has no matching action", found.GetCardId())
	}
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: &v1.CardAction{
		CardId: found.GetCardId(), CallId: found.GetCallId(), Action: action, Values: a.Values, Seq: seq}}}}, nil
}
