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
	"yanshi/internal/askuser"
	"yanshi/internal/call"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/feed"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/realtime"
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
	calls   *call.Manager
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
		Index: lifecycle.NewMemIndex(), Deletions: lifecycle.NewMemDeletions(), Artifacts: arts}
	idle := &workqueue.IdleGate{Ready: queue.Ready()}
	bus := live.NewMemBus()
	pool := &workerPool{ctx: ctx, workers: map[string]*runtime.Worker{}, newCfg: func(id string) runtime.Config {
		return runtime.Config{ID: id, Store: store, Queue: queue, Agents: agents,
			Model: cfg.Model, Catalog: catalog, Dispatch: router, Artifacts: arts, Memory: mems, Logger: cfg.Logger,
			LeaseTTL: cfg.LeaseTTL, Heartbeat: cfg.LeaseTTL / 3, Idle: idle, Live: bus}
	}}
	for range 2 * cfg.Parallel {
		pool.spawn()
	}
	if cfg.Sandbox != nil {
		c := &sandbox.Controller{ID: "eval-sandbox", Queue: sbxQueue, Hub: hub, Provider: cfg.Sandbox, Activity: sandbox.NewMemActivity(),
			Ledger: nodesdk.NewMemLedger(), Artifacts: arts, Clock: clk, Logger: cfg.Logger, LeaseTTL: 30 * time.Second, Heartbeat: 10 * time.Second}
		go c.Run(ctx)
	}
	in := &instance{cfg: cfg, svc: svc, hub: hub, mems: mems, agents: agents, version: versions, workers: pool, arts: arts,
		calls: &call.Manager{Service: svc, Realtime: cfg.Realtime, Feed: feed.Source{Log: store.Log, Live: bus}, Logger: cfg.Logger}}
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
	// 预置记忆的 ID 与生产一致不可猜（由随机调用 ID 派生）：顺序 ID 会让模型按规律"猜出"未见过的 ID，
	// 替换掉另一条记忆（memory-update-crowded 中观察到）。
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
		if _, err := in.mems.Save(tctx, memory.SaveRequest{BusinessLine: BusinessLine, EndUser: endUser, CallID: "setup_" + ids.Random()(),
			Category: memory.Category(m.Category), Content: m.Content, At: at}); err != nil {
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
			id, err := in.svc.Create(tctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: endUser, Agent: c.Agent, AgentVersion: in.version[c.Name]})
			if err != nil {
				tr.Err = "create session: " + err.Error()
				return tr
			}
			sid = id
			sessions = append(sessions, id)
			if c.Call {
				if voice, rec, err = in.startCall(tctx, sid); err != nil {
					tr.Err = "start call: " + err.Error()
					return tr
				}
			}
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
	tickets                []string // CRM 中本 EndUser 的工单
	inTokens, cachedTokens uint64   // 本轮模型调用的输入 token 与其中命中前缀缓存的部分
	callArgs               []string // 与 calls 一一对应的调用参数
	tasks                  []string // Call 中派生的任务（run_task 的 task）
}

// turn 提交一轮输入，按 approve 自动作出审批决定，等待 Run 结束并收集本轮的事件。
func (in *instance) turn(ctx context.Context, sid string, turn Turn, timeout time.Duration) (*observation, error) {
	st, err := in.svc.Load(ctx, sid)
	if err != nil {
		return nil, err
	}
	after := st.Seq
	input := model.TextBlocks(turn.Input)
	if u := turn.UIContext; u != nil {
		input = append([]*v1.ContentBlock{{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{
			Screen: u.Screen, Ref: u.Ref, Selection: u.Selection, Content: u.Content}}}}, input...)
	}
	res, err := in.svc.Submit(ctx, sid, input)
	if err != nil {
		return nil, err
	}
	approve := turn.Approve == nil || *turn.Approve
	if timeout <= 0 {
		timeout = in.cfg.TurnTimeout
	}
	deadline := time.Now().Add(timeout)
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
		run = st.Run(res.RunID)
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
	if asked {
		obs.status = "asked"
	}
	collect(events, obs)
	return obs, nil
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
	return (want.Category == "" || string(m.Category) == want.Category) && strings.Contains(m.Content, want.Contains) &&
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
	for g, sub := range e.CallArgs {
		ok := false
		for i, c := range o.calls {
			if matches([]string{g}, c) && strings.Contains(o.callArgs[i], sub) {
				ok = true
			}
		}
		add("call_args "+g, ok, fmt.Sprintf("calls %v", o.calls))
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
