package eval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
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

// BusinessLine 是评测使用的业务线。
const BusinessLine = "eval"

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
}

// workerPool 管理 Worker，支持"杀掉"一个 Worker 并补充新的（CrashAfterCalls）。
type workerPool struct {
	mu      sync.Mutex
	ctx     context.Context
	cancels map[string]context.CancelFunc
	next    int
	newCfg  func(id string) runtime.Config
}

func (p *workerPool) spawn() {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := fmt.Sprintf("eval-worker-%d", p.next)
	p.next++
	ctx, cancel := context.WithCancel(p.ctx)
	p.cancels[id] = cancel
	w := runtime.New(p.newCfg(id))
	go w.Run(ctx)
}

// crash 取消 Worker 的上下文：进行中的模型调用或能力调用被中断，租约不释放，与进程崩溃相同。
// 随后补充一个新 Worker，保持并发度。
func (p *workerPool) crash(id string) bool {
	p.mu.Lock()
	cancel, ok := p.cancels[id]
	delete(p.cancels, id)
	p.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
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
	in, err := start(ctx, cfg, cases)
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
	return rep, ctx.Err()
}

func start(ctx context.Context, cfg Config, cases []*Case) (*instance, error) {
	clk := clock.Real{}
	store := &session.Store{Log: memlog.New(), IDs: ids.Random(), Clock: clk, Snapshots: session.NewMemSnapshots()}
	queue := memqueue.New(clk)
	dir, inbox := node.NewMemDirectory(clk), node.NewMemInbox()
	hub := &node.Hub{Dir: dir, Inbox: inbox, Store: store, Queue: queue, Auth: node.InsecureDevAuth{}}
	sbxQueue := memqueue.New(clk)
	router := &sandbox.Router{Hub: hub, Queue: sbxQueue}
	arts := &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: artifact.NewMemBlobs(), IDs: ids.Random(), Clock: clk}
	// 评测业务线视为已取得健康信息的单独同意，以评测模型对健康信息的处理（ADR-0022）。
	mems := &memory.Service{Store: memory.NewMemStore(), Grants: memory.NewMemGrants(), Clock: clk, IDs: ids.Random(),
		HealthAllowed: func(bl string) bool { return bl == BusinessLine }}
	if cfg.EmbedModel != "" {
		mems.Embedder, mems.EmbedModel = cfg.Model, cfg.EmbedModel
	}
	catalog := &capability.Catalog{
		Local: capability.NewRegistry(append([]capability.Capability{capability.ClockNow(clk)}, memory.Capabilities(mems)...)...),
		Nodes: dir, DefaultTimeout: 10 * time.Minute,
	}
	if cfg.Sandbox != nil {
		catalog.Sandbox = &capability.SandboxTools{Specs: sandbox.Specs(), NodeID: sandbox.NodeID}
	}
	agents, versions, err := derive(cfg, cases)
	if err != nil {
		return nil, err
	}
	svc := &service.Service{Store: store, Queue: queue, Agents: agents, Nodes: router,
		Index: lifecycle.NewMemIndex(), Deletions: lifecycle.NewMemDeletions()}
	idle := &workqueue.IdleGate{Ready: queue.Ready()}
	pool := &workerPool{ctx: ctx, cancels: map[string]context.CancelFunc{}, newCfg: func(id string) runtime.Config {
		return runtime.Config{ID: id, Store: store, Queue: queue, Agents: agents,
			Model: cfg.Model, Catalog: catalog, Dispatch: router, Artifacts: arts, Memory: mems, Logger: cfg.Logger,
			LeaseTTL: cfg.LeaseTTL, Heartbeat: cfg.LeaseTTL / 3, Idle: idle}
	}}
	for range 2 * cfg.Parallel {
		pool.spawn()
	}
	if cfg.Sandbox != nil {
		c := &sandbox.Controller{ID: "eval-sandbox", Queue: sbxQueue, Hub: hub, Provider: cfg.Sandbox, Activity: sandbox.NewMemActivity(),
			Ledger: nodesdk.NewMemLedger(), Artifacts: arts, Clock: clk, Logger: cfg.Logger, LeaseTTL: 30 * time.Second, Heartbeat: 10 * time.Second}
		go c.Run(ctx)
	}
	in := &instance{cfg: cfg, svc: svc, hub: hub, mems: mems, agents: agents, version: versions, workers: pool, arts: arts}
	if slices.ContainsFunc(cases, func(c *Case) bool { return c.Setup.CRM }) {
		crm, err := startCRM(ctx, arts, cfg.Logger)
		if err != nil {
			return nil, err
		}
		in.crm, catalog.MCP = crm, crm.connector
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
	Err                           string
}

type Assertion struct {
	Turn   int
	Name   string
	Pass   bool
	Detail string
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
	for i, m := range c.Setup.Memories {
		if _, err := in.mems.Save(tctx, memory.SaveRequest{BusinessLine: BusinessLine, EndUser: endUser, CallID: fmt.Sprintf("setup-%s-%d", endUser, i),
			Category: memory.Category(m.Category), Content: m.Content}); err != nil {
			tr.Err = "setup memory: " + err.Error()
			return tr
		}
	}
	var dev *device
	if s := c.Setup.Device; s != nil {
		dev = &device{files: map[string]string{}, writes: map[string]string{}, arts: in.arts}
		for k, v := range s.Files {
			dev.files[k] = v
		}
		label := s.Label
		if label == "" {
			label = "macbook"
		}
		if err := dev.run(tctx, in.hub, "node-"+endUser, BusinessLine, endUser, label, s.OnlineAfter); err != nil {
			tr.Err = "setup device: " + err.Error()
			return tr
		}
	}
	var sid string
	var sessions []string
	if in.cfg.LogDir != "" {
		defer func() { in.dumpLogs(ctx, fmt.Sprintf("%s-%d", c.Name, trial+1), sessions) }()
	}
	for ti, turn := range c.Turns {
		if sid == "" || turn.NewSession {
			id, err := in.svc.Create(tctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: endUser, Agent: c.Agent, AgentVersion: in.version[c.Name]})
			if err != nil {
				tr.Err = "create session: " + err.Error()
				return tr
			}
			sid = id
			sessions = append(sessions, id)
		}
		obs, err := in.turn(tctx, sid, turn, c.Timeout)
		if err != nil {
			tr.Err = fmt.Sprintf("turn %d: %v", ti+1, err)
			return tr
		}
		tr.Tokens += obs.tokens
		tr.Calls, tr.Compactions, tr.Takeovers = tr.Calls+len(obs.calls), tr.Compactions+obs.compactions, tr.Takeovers+obs.takeovers
		if dev != nil {
			obs.writes, obs.sent = dev.snapshot()
		}
		obs.tickets = in.crm.ticketsOf(endUser)
		obs.memories, _ = in.mems.Store.List(tctx, endUser, []memory.Scope{{BusinessLine: BusinessLine}}, memory.DefaultMaxPerUser)
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
	reply     string
	tokens    uint64
	steps     []string // 给评分模型看的调用摘要
	writes    map[string]string
	sent      int
	memories  []*memory.Memory
	compacted bool // 到本轮结束时 Session 是否发生过上下文压缩
	// compactions 是本轮的上下文压缩次数，takeovers 是本轮 Run 的接管次数。
	compactions, takeovers int
	tickets                []string // CRM 中本 EndUser 的工单
}

// turn 提交一轮输入，按 approve 自动作出审批决定，等待 Run 结束并收集本轮的事件。
func (in *instance) turn(ctx context.Context, sid string, turn Turn, timeout time.Duration) (*observation, error) {
	st, err := in.svc.Load(ctx, sid)
	if err != nil {
		return nil, err
	}
	after := st.Seq
	res, err := in.svc.Submit(ctx, sid, model.TextBlocks(turn.Input))
	if err != nil {
		return nil, err
	}
	approve := turn.Approve == nil || *turn.Approve
	if timeout <= 0 {
		timeout = in.cfg.TurnTimeout
	}
	deadline := time.Now().Add(timeout)
	crashed := false
	var run *session.Run
	var compacted bool
	for {
		st, err := in.svc.Load(ctx, sid)
		if err != nil {
			return nil, err
		}
		run = st.Run(res.RunID)
		if run != nil && run.Status.Terminal() {
			compacted = st.Compaction != nil
			break
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
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("run %s did not finish within %s", res.RunID, timeout)
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
	obs := &observation{input: turn.Input, status: run.Status.String(), compacted: compacted, takeovers: run.Takeovers}
	results := map[string]string{}
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *v1.Event_AssistantMessage:
			m := p.AssistantMessage
			obs.tokens += m.GetUsage().GetInputTokens() + m.GetUsage().GetOutputTokens()
			for _, tc := range m.GetToolCalls() {
				obs.calls = append(obs.calls, tc.GetCapability())
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
			obs.tokens += p.ContextCompacted.GetUsage().GetInputTokens() + p.ContextCompacted.GetUsage().GetOutputTokens()
		}
	}
	return obs, nil
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
	return (want.Category == "" || string(m.Category) == want.Category) && strings.Contains(m.Content, want.Contains)
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
	add("status", o.status == want, fmt.Sprintf("status %s, want %s", o.status, want))
	for _, g := range e.Calls {
		add("calls "+g, slices.ContainsFunc(o.calls, func(c string) bool { return matches([]string{g}, c) }), fmt.Sprintf("calls %v", o.calls))
	}
	for _, g := range e.NoCalls {
		add("no_calls "+g, !slices.ContainsFunc(o.calls, func(c string) bool { return matches([]string{g}, c) }), fmt.Sprintf("calls %v", o.calls))
	}
	if r := e.Approvals; r != nil {
		ok := (r.Min == nil || o.approvals >= *r.Min) && (r.Max == nil || o.approvals <= *r.Max)
		add("approvals", ok, fmt.Sprintf("approvals %d", o.approvals))
	}
	for _, s := range e.ReplyContains {
		add("reply_contains "+s, strings.Contains(o.reply, s) || strings.Contains(plainNumbers(o.reply), s), "reply: "+clip(o.reply, 200))
	}
	if e.ReplyMaxChars > 0 {
		n := utf8.RuneCountInString(o.reply)
		add("reply_max_chars", n <= e.ReplyMaxChars, fmt.Sprintf("reply has %d chars", n))
	}
	for _, m := range e.Memories {
		ok := slices.ContainsFunc(o.memories, func(x *memory.Memory) bool { return memoryMatches(x, m) })
		add(fmt.Sprintf("memory %s %q", m.Category, m.Contains), ok, fmt.Sprintf("%d memories", len(o.memories)))
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
	for _, want := range e.Tickets {
		add("tickets "+want, slices.ContainsFunc(o.tickets, func(t string) bool { return strings.Contains(t, want) }), fmt.Sprintf("tickets %v", o.tickets))
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
			add("judge", false, "judge error: "+err.Error())
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

var digitGroup = regexp.MustCompile(`(\d)[,，](\d{3})`)

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
