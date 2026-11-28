// Package runtime 是执行 Run 的无状态 Worker。
//
// Worker 以 Step 为单位推进：每次 Step 同步完成一件事（认领、开始 Attempt、
// 一次模型调用、一次 Capability 调用的一个阶段），内部不启动影响结果的 goroutine，
// 以便确定性模拟测试在任意两步之间注入故障。语义见 docs/design/m0-core-primitives.md。
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/decision"
	"yanshi/internal/faults"
	"yanshi/internal/live"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
	"yanshi/internal/notify"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

// Dispatcher 把路由调用投递到 Node 的 Inbox（由 sandbox.Router 实现）。class 是 Session 的工作类别：
// 执行调用的队列（如沙箱队列）沿用它（ADR-0029）。
type Dispatcher interface {
	Dispatch(ctx context.Context, endUser, nodeID string, inv *v1.Invoke, class workqueue.Class) error
	Cancel(ctx context.Context, endUser, nodeID, callID string) error
}

// Devices 查询 Node 目录中设备的在线状态（node.Directory 的子集），用于判断路由调用是否转入后台（ADR-0035）。
type Devices interface {
	Get(ctx context.Context, endUser, nodeID string) (*node.Info, error)
}

// ScheduleFirer 在到点后原子记录进度与输入，Worker 仅在没有既有 Run 时调用。
type ScheduleFirer interface {
	FireSchedule(ctx context.Context, st *session.State, scheduleID string) error
}

// BackgroundSettler 了结到期的后台调用（由 service.Service 实现）：写入 reason = timeout 的后台结果输入，并入活跃 Run 或开启
// 新 Run（与设备结果相同的规则），st 随之同步。调用已了结或尚未到期时不写入。
type BackgroundSettler interface {
	ExpireBackground(ctx context.Context, st *session.State, callID string) error
}

// Reporter 把委托派生的子 Session 中 Run 的终态写回父调用（delegation.Delegator，docs/design/m5-multi-agent.md）。
type Reporter interface {
	Report(ctx context.Context, child *session.State) error
	// Reconcile 在父调用不再等待时中断子 Session 中进行中的委托 Run，返回 true 表示已中断。
	Reconcile(ctx context.Context, child *session.State) (bool, error)
}

type Config struct {
	ID     string
	Store  *session.Store
	Queue  workqueue.Queue
	Agents *agentdef.Registry
	Model  model.Provider
	// Loop 是内置循环，决定每一步的回复与调用请求；nil 时使用以 Model 为后端的 ModelLoop。
	Loop Loop
	// Loops 解析 AgentDef 指向的外部循环登记（ADR-0062）；nil 时这样的 AgentDef 的 Run 以配置错误失败。
	Loops Loops
	// Compactor 生成上下文压缩摘要；nil 时使用以 Model 为后端的 ModelCompactor。
	Compactor Compactor
	Catalog   *capability.Catalog
	Dispatch  Dispatcher
	// Delegation 非空时，委托派生的子 Session 在 Run 结束后把终态写回父调用。
	Delegation Reporter
	// InjectionCheck 非空时，以影子模式判断来自外部的调用结果（设备、沙箱、MCP、委托）是否含提示注入，结果记为
	// DecisionMade，不影响执行（docs/design/m6-decisions.md §6 第 2 步，ADR-0030）。
	InjectionCheck decision.Decider
	// Memory 非空且 AgentDef 配置了召回时，每个 Run 开始先召回 Memory（docs/design/m4-memory-grant.md §3）。
	Memory Recaller
	// Artifacts 非空时，用户消息中的图片工件会内联给模型（docs/design/m2-artifacts.md §4）。
	Artifacts *artifact.Service
	Live      live.Bus
	// LiveEndpoint 是本进程的内部地址，写入 AttemptStarted，供其他进程拉取增量（ADR-0013）。
	LiveEndpoint string
	Logger       *slog.Logger
	Tracer       *tracing.Tracer

	LeaseTTL time.Duration
	// Heartbeat > 0 时，在模型调用与 Capability 执行期间按此间隔续约（真实时间）。
	// 模拟测试置 0。
	Heartbeat time.Duration
	// MaxTakeovers 是一个 Run 在没有进展的情况下最多被连续接管的次数，超过则 RunFailed
	// （docs/design/m2-long-runs.md §6）。从挂起恢复不计入。
	MaxTakeovers int
	// ApprovalTimeout 是审批的等待时长。
	ApprovalTimeout time.Duration
	// MaxModelErrors 是同一 Attempt 内连续模型错误的上限，超过则 RunFailed。
	MaxModelErrors int
	// StepErrorBudget 是同一 Run 的 Step 连续出错多久后判为失败（默认 2 分钟）。避免确定性的错误
	// （如无法序列化的事件）使 Run 永远停在 running：用户既得不到结果，也看不到失败。
	StepErrorBudget time.Duration
	// IdleWait 是 Run 循环在无事可做时的等待时间（未配置 Idle 时）。
	IdleWait time.Duration
	// Idle 非空时，同一进程的 Worker 共享它：空闲时至多一个 Worker 轮询队列（见 workqueue.IdleGate）。
	Idle *workqueue.IdleGate
	// Pool 是本 Worker 认领的工作（ADR-0029），零值认领任意工作。持有的 Run 不再属于本池时（如交互 Worker
	// 手中的 Run 成为长任务），在两步之间移交给其他 Worker。
	Pool workqueue.Pool
	// DrainTimeout 是优雅停机时等待当前这一步完成的上限（默认 20 秒），之后取消它并移交 Run。
	DrainTimeout time.Duration
	// Meter 非空时记录每次模型调用的用量；Quotas 非空时在每次模型调用前检查配额
	// （docs/design/m4-quota-usage.md）。
	Meter  *usage.Meter
	Quotas *usage.Quotas
	// Moderator 非空时，模型输出（回复文本与调用参数）在写日志之前检查（docs/design/m4-moderation.md）。
	Moderator moderation.Moderator
	// QuotaRecheck 是因配额挂起的 Run 重新检查的最长间隔（默认 10 分钟），使调高配额后及时恢复。
	QuotaRecheck time.Duration
	// 输出审核不可用时 Run 挂起等待（ADR-0054）：第一次等 ModerationRetry（默认 30 秒），之后每次翻倍，至多
	// ModerationRetryMax（默认 10 分钟）；从第一次挂起起累计超过 ModerationWaitMax（默认 6 小时）仍不可用时，
	// Run 以明确的原因失败，不放行。
	ModerationRetry    time.Duration
	ModerationRetryMax time.Duration
	ModerationWaitMax  time.Duration
	// Notify 非空时，写入需要 EndUser 注意的事件之后发出提醒：待审批、待回答的提问、持续超过
	// NotifyRunsAfter（默认 2 分钟）的 Run 结束（docs/design/m3-duplex-channel.md §9）。
	Notify          Notifier
	NotifyRunsAfter time.Duration
	// Faults 非空时按规则注入 Worker 崩溃（serve -faults 的调试部署，ADR-0033）；模型、内容安全与设备目录的故障
	// 由各自的包装注入（internal/faults）。
	Faults *faults.Injector
	// 后台调用（ADR-0035）：Devices 非空时，路由到设备的调用已派发、设备离线超过 BackgroundGrace（默认 20 秒，从派发与
	// 设备下线中较晚者起算）仍无结果，就转入后台：Run 得到占位结果继续推进，调用在 Session 级等待到 BackgroundTimeout
	// （默认 24 小时）。Background 了结到期的后台调用；Session 有未了结的后台调用时，Worker 停放到最早的截止时间。
	Devices           Devices
	Background        BackgroundSettler
	Schedules         ScheduleFirer
	BackgroundGrace   time.Duration
	BackgroundTimeout time.Duration
}

// Notifier 处理提醒（由 notify.Notifier 实现）。它不得阻塞 Step：推送经网络，应在后台进行。
type Notifier interface {
	Notify(ctx context.Context, n notify.Notification)
}

// 后台调用的默认宽限期与后台时限（ADR-0035）。宽限期覆盖短暂的断网与切到后台：期间上线的设备照常在 Run 中返回结果；
// 后台时限覆盖"一天之内总会打开一次"的移动设备。
const (
	DefaultBackgroundGrace   = 20 * time.Second
	DefaultBackgroundTimeout = 24 * time.Hour
)

func (c *Config) defaults() {
	if c.LeaseTTL == 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.MaxTakeovers == 0 {
		c.MaxTakeovers = 4
	}
	if c.ApprovalTimeout == 0 {
		c.ApprovalTimeout = time.Hour
	}
	if c.MaxModelErrors == 0 {
		c.MaxModelErrors = 3
	}
	if c.IdleWait == 0 {
		c.IdleWait = 100 * time.Millisecond
	}
	if c.StepErrorBudget == 0 {
		c.StepErrorBudget = 2 * time.Minute
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = 20 * time.Second
	}
	if c.NotifyRunsAfter == 0 {
		c.NotifyRunsAfter = 2 * time.Minute
	}
	if c.QuotaRecheck == 0 {
		c.QuotaRecheck = 10 * time.Minute
	}
	if c.ModerationRetry == 0 {
		c.ModerationRetry = 30 * time.Second
	}
	if c.ModerationRetryMax == 0 {
		c.ModerationRetryMax = 10 * time.Minute
	}
	if c.ModerationWaitMax == 0 {
		c.ModerationWaitMax = 6 * time.Hour
	}
	if c.BackgroundGrace == 0 {
		c.BackgroundGrace = DefaultBackgroundGrace
	}
	if c.BackgroundTimeout == 0 {
		c.BackgroundTimeout = DefaultBackgroundTimeout
	}
	if c.Live == nil {
		c.Live = live.Discard{}
	}
	if c.Loop == nil {
		c.Loop = ModelLoop{Model: c.Model}
	}
	if c.Compactor == nil {
		c.Compactor = ModelCompactor{Model: c.Model}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

type Worker struct {
	cfg   Config
	lease *workqueue.Lease
	st    *session.State
	// 本 Worker 当前持有的 Attempt。
	// renewed 是上次续约（或认领）的时间。
	renewed     time.Time
	runID       string
	attempt     uint32
	attemptSpan *tracing.Span
	modelErrors int
	// forceCompact 在模型报告上下文超长后置位：下一步无论估算如何都先压缩。
	forceCompact bool
	// failingSince 是当前 Run 的 Step 开始连续出错的时间；零值表示上一步成功。
	failingSince time.Time
	// executed 是本 Attempt 已执行完、结果尚未写入日志的进程内调用（按 call_id）：写入失败后下一步只重试写入，
	// 不再执行，否则非幂等的能力（如已批准的 MCP 写操作）会再次产生副作用。
	executed map[string]*v1.Event

	// killed 由 Kill 置位：Run 立即退出、不移交；kill 取消进行中的这一步。
	killed atomic.Bool
	killMu sync.Mutex
	kill   context.CancelFunc
}

func New(cfg Config) *Worker {
	cfg.defaults()
	return &Worker{cfg: cfg}
}

func (w *Worker) ID() string { return w.cfg.ID }

// Run 持续执行 Step 直到 ctx 结束，然后优雅停机（ADR-0029）：不再认领新工作，完成进行中的这一步
// （最多等 DrainTimeout），再移交持有的 Run，其他进程立即接手，不必等租约过期。
func (w *Worker) Run(ctx context.Context) {
	// 这一步不随 ctx 取消：进行中的模型调用写完日志再停，避免接手的 Worker 重做整次调用。
	stepCtx, cancelStep := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelStep()
	w.killMu.Lock()
	w.kill = cancelStep
	w.killMu.Unlock()
	drain := context.AfterFunc(ctx, func() { time.AfterFunc(w.cfg.DrainTimeout, cancelStep) })
	defer drain()
	defer func() {
		if w.killed.Load() {
			w.endAttempt(tracing.Lost)
			return
		}
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := w.Stop(hctx); err != nil {
			w.cfg.Logger.Warn("worker hand-off failed", "worker", w.cfg.ID, "err", err)
		}
	}()
	for ctx.Err() == nil && !w.killed.Load() {
		if w.lease == nil && w.cfg.Idle != nil {
			w.cfg.Idle.Idle(ctx, func() bool {
				did, err := w.Step(stepCtx)
				if err != nil && ctx.Err() == nil {
					w.cfg.Logger.Warn("worker step failed", "worker", w.cfg.ID, "err", err)
				}
				// 认领到工作（持有租约）或有进展时离开门控；出错时留在门控内退避，避免空转。
				return w.lease != nil || (did && err == nil)
			})
			continue
		}
		did, err := w.Step(stepCtx)
		if err != nil && ctx.Err() == nil {
			w.cfg.Logger.Warn("worker step failed", "worker", w.cfg.ID, "err", err)
		}
		if !did || err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(w.cfg.IdleWait):
			}
		}
	}
}

// Kill 模拟进程崩溃（测试与评测用）：立即取消进行中的这一步，Run 退出时不移交，租约留到过期后被接管。
func (w *Worker) Kill() {
	w.killed.Store(true)
	w.killMu.Lock()
	defer w.killMu.Unlock()
	if w.kill != nil {
		w.kill()
	}
}

// Step 推进一件工作；返回 false 表示当前无事可做。
func (w *Worker) Step(ctx context.Context) (bool, error) {
	var claimStart time.Time
	if w.lease == nil {
		if w.cfg.Tracer.Enabled() {
			claimStart = time.Now()
		}
		l, err := w.cfg.Queue.Claim(ctx, w.cfg.ID, w.cfg.LeaseTTL, w.cfg.Pool)
		if errors.Is(err, workqueue.ErrEmpty) {
			metrics.QueueClaims.WithLabelValues("empty").Inc()
			return false, nil
		}
		if err != nil {
			metrics.QueueClaims.WithLabelValues("error").Inc()
			return false, err
		}
		metrics.QueueClaims.WithLabelValues("ok").Inc()
		metrics.QueueWait.WithLabelValues("sessions", l.Class.Priority.String()).Observe(l.Waited.Seconds())
		w.drop()
		w.lease, w.renewed = l, w.now()
	} else if w.now().Sub(w.renewed) >= w.cfg.LeaseTTL/3 {
		// 距上次续约不足 TTL/3 时租约必然仍有效，跳过续约以减少每步的数据库往返（m2-scale-test）。
		if err := w.cfg.Queue.Renew(ctx, w.lease, w.cfg.LeaseTTL); err != nil {
			w.drop()
			if errors.Is(err, workqueue.ErrLeaseLost) {
				return true, nil
			}
			return false, err
		}
		w.renewed = w.now()
	}

	if w.st == nil {
		start := time.Now()
		st, err := w.cfg.Store.Load(ctx, w.lease.SessionID)
		if err == nil {
			metrics.SessionLoad.WithLabelValues().Observe(metrics.Since(start))
			w.st = st
		}
		if !claimStart.IsZero() {
			outcome := tracing.OK
			if err != nil {
				outcome = tracing.Error
			}
			w.cfg.Tracer.Record(tracing.QueueClaim, claimStart, time.Now(), outcome,
				tracing.Session(w.lease.SessionID), tracing.Worker(w.cfg.ID), tracing.QueueWait(w.lease.Waited))
		}
		if err != nil {
			return false, err
		}
	} else if !w.upToDate() {
		if err := w.cfg.Store.Sync(ctx, w.st); err != nil {
			return false, err
		}
	}
	w.observeAttempt()

	// 到期的后台调用先了结（ADR-0035）：它作为一条输入并入活跃 Run 或开启新 Run，随后照常推进。
	if b := w.dueBackground(); b != nil {
		return true, w.expireBackground(ctx, b)
	}
	r := w.st.Active()
	if r == nil {
		// Active 包含排队输入；只在所有既有 Run 终态后处理一个定时。
		if open := w.st.OpenSchedules(); w.cfg.Schedules != nil && len(open) > 0 && !open[0].NextDue.After(w.now()) {
			err := w.cfg.Schedules.FireSchedule(ctx, w.st, open[0].Created.GetScheduleId())
			if errors.Is(err, session.ErrGone) {
				w.drop()
				return true, nil
			}
			return true, err
		}
		// 委托派生的子 Session：先把终态写回父调用，再归还。写回之前崩溃时租约留在队列中，接手的 Worker 走到这里
		// 再写一次；写回幂等（父调用已有结果时作废）。
		if w.st.Created.GetParent() != nil && w.cfg.Delegation != nil {
			if err := w.cfg.Delegation.Report(ctx, w.st); err != nil {
				return true, err
			}
		}
		// 有未了结的后台调用时不能移出队列：没有人会在截止时间叫醒它。停放到最早的截止时间，期间设备结果、
		// 用户输入照常唤醒。
		if until, ok := w.st.NextBackgroundDeadline(); ok {
			return true, w.park(ctx, until)
		}
		if until, ok := w.st.NextScheduleDue(); ok {
			return true, w.park(ctx, until)
		}
		return true, w.release(ctx, true)
	}
	if !w.cfg.Pool.Admits(w.st.WorkClass()) {
		return true, w.handOff(ctx, r, "pool")
	}
	if !w.owns(r) {
		if r.QuotaSuspended() {
			if parked, err := w.reparkOverQuota(ctx); parked || err != nil {
				return true, err
			}
		}
		// 因审核不可用挂起、退避尚未到期时被其他事件（插话、后台调用等）唤醒：直接重新停放，不开始 Attempt、
		// 不写事件，免得每次唤醒都重新调用模型（ADR-0054）。中断与删除使 Run 不再活跃，不会走到这里。
		if r.ModerationSuspended() && w.now().Before(r.SuspendedUntil) {
			return true, w.park(ctx, r.SuspendedUntil)
		}
		// 并行委托中的一个子 Run 写回了结果、或有插话，其余委托还没完成：不开始 Attempt、不写事件，按 call_id 幂等地
		// 重新派发后重新停放（ADR-0055）。重新派发不能省：派发是子任务丢失时的恢复路径，子 Session 被删除、上次的
		// 回写失败时，由它向父调用写回错误结果（模拟测试发现）。
		if until, ok := w.awaitingDelegations(r); ok && r.Status == session.RunSuspended && r.SuspendReason == "" {
			_, err := w.deliverAndCommit(ctx, r, deliveries(r.DelegationFront()), nil, until)
			return true, err
		}
		return true, w.startAttempt(ctx, r)
	}
	if w.st.WorkClass().Priority == workqueue.Background {
		ctx = model.WithBackground(ctx) // 长任务的模型调用在提供商准入处让交互调用先行（ADR-0029）
	}
	ctx = faults.WithSession(ctx, w.st.SessionID)
	err := w.advance(ctx, r)
	if err == nil && w.cfg.Faults != nil {
		if _, crash := w.cfg.Faults.Take(ctx, faults.WorkerCrash); crash {
			// 注入的崩溃（ADR-0033）：这一步已写入日志，Worker 不释放租约、不移交，Run 在租约过期后被接管。
			w.cfg.Logger.Warn("injected worker crash", "worker", w.cfg.ID, "session", w.st.SessionID, "run", r.ID)
			w.drop()
			return true, nil
		}
	}
	if err == nil || ctx.Err() != nil {
		w.failingSince = time.Time{}
		return true, err
	}
	if w.failingSince.IsZero() {
		w.failingSince = w.now()
	} else if w.now().Sub(w.failingSince) >= w.cfg.StepErrorBudget {
		w.failingSince = time.Time{}
		return true, w.fail(ctx, r, "internal error: "+err.Error())
	}
	return true, err
}

func (w *Worker) now() time.Time { return w.cfg.Store.Clock.Now() }

func (w *Worker) advance(ctx context.Context, r *session.Run) error {
	def, err := w.cfg.Agents.Get(w.st.Agent)
	if err != nil {
		return w.fail(ctx, r, err.Error())
	}
	if front := r.DelegationFront(); len(front) > 0 {
		return w.advanceDelegations(ctx, r, def, front)
	}
	if c := r.PendingCall(); c != nil {
		return w.advanceCall(ctx, r, def, c)
	}
	if r.Turns >= def.MaxTurns {
		return w.fail(ctx, r, fmt.Sprintf("exceeded %d turns", def.MaxTurns))
	}
	if w.cfg.Memory != nil && def.Memory.Recall > 0 && !r.Recalled && r.Turns == 0 {
		return w.recall(ctx, r, def)
	}
	// 委托派生的子 Session：每次调用模型之前确认父调用仍在等待，不在等了就停下，不再消耗配额。
	if w.st.Created.GetParent() != nil && w.cfg.Delegation != nil {
		if stopped, err := w.cfg.Delegation.Reconcile(ctx, w.st); err != nil || stopped {
			return err
		}
	}
	if w.cfg.InjectionCheck != nil {
		if pending := w.unjudged(r); len(pending) > 0 {
			return w.judgeResults(ctx, r, pending)
		}
	}
	return w.callModel(ctx, r, def)
}
