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
	"io"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/eventlog"
	"yanshi/internal/live"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

// Dispatcher 把路由调用投递到 Node 的 Inbox（由 node.Hub 实现）。
type Dispatcher interface {
	Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke) error
	Cancel(ctx context.Context, nodeID, callID string) error
}

type Config struct {
	ID     string
	Store  *session.Store
	Queue  workqueue.Queue
	Agents *agentdef.Registry
	Model  model.Provider
	// Compactor 生成上下文压缩摘要；nil 时使用以 Model 为后端的 ModelCompactor。
	Compactor Compactor
	Catalog   *capability.Catalog
	Dispatch  Dispatcher
	// Memory 非空且 AgentDef 配置了召回时，每个 Run 开始先召回 Memory（docs/design/m4-memory-grant.md §3）。
	Memory Recaller
	// Artifacts 非空时，用户消息中的图片工件会内联给模型（docs/design/m2-artifacts.md §4）。
	Artifacts *artifact.Service
	Live      live.Bus
	// LiveEndpoint 是本进程的内部地址，写入 AttemptStarted，供其他进程拉取增量（ADR-0013）。
	LiveEndpoint string
	Logger       *slog.Logger

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
	// Meter 非空时记录每次模型调用的用量；Quotas 非空时在每次模型调用前检查配额
	// （docs/design/m4-quota-usage.md）。
	Meter  *usage.Meter
	Quotas *usage.Quotas
	// Moderator 非空时，模型输出（回复文本与调用参数）在写日志之前检查（docs/design/m4-moderation.md）。
	Moderator moderation.Moderator
	// QuotaRecheck 是因配额挂起的 Run 重新检查的最长间隔（默认 10 分钟），使调高配额后及时恢复。
	QuotaRecheck time.Duration
}

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
	if c.QuotaRecheck == 0 {
		c.QuotaRecheck = 10 * time.Minute
	}
	if c.Live == nil {
		c.Live = live.Discard{}
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
	modelErrors int
	// forceCompact 在模型报告上下文超长后置位：下一步无论估算如何都先压缩。
	forceCompact bool
	// failingSince 是当前 Run 的 Step 开始连续出错的时间；零值表示上一步成功。
	failingSince time.Time
}

func New(cfg Config) *Worker {
	cfg.defaults()
	return &Worker{cfg: cfg}
}

func (w *Worker) ID() string { return w.cfg.ID }

// Run 持续执行 Step 直到 ctx 结束。
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if w.lease == nil && w.cfg.Idle != nil {
			w.cfg.Idle.Idle(ctx, func() bool {
				did, err := w.Step(ctx)
				if err != nil && ctx.Err() == nil {
					w.cfg.Logger.Warn("worker step failed", "worker", w.cfg.ID, "err", err)
				}
				// 认领到工作（持有租约）或有进展时离开门控；出错时留在门控内退避，避免空转。
				return w.lease != nil || (did && err == nil)
			})
			continue
		}
		did, err := w.Step(ctx)
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

func (w *Worker) drop() {
	w.lease, w.st, w.runID, w.attempt, w.modelErrors, w.forceCompact, w.failingSince = nil, nil, "", 0, 0, false, time.Time{}
}

// Step 推进一件工作；返回 false 表示当前无事可做。
func (w *Worker) Step(ctx context.Context) (bool, error) {
	if w.lease == nil {
		l, err := w.cfg.Queue.Claim(ctx, w.cfg.ID, w.cfg.LeaseTTL)
		if errors.Is(err, workqueue.ErrEmpty) {
			metrics.QueueClaims.WithLabelValues("empty").Inc()
			return false, nil
		}
		if err != nil {
			metrics.QueueClaims.WithLabelValues("error").Inc()
			return false, err
		}
		metrics.QueueClaims.WithLabelValues("ok").Inc()
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
		if err != nil {
			return false, err
		}
		metrics.SessionLoad.WithLabelValues().Observe(metrics.Since(start))
		w.st = st
	} else if !w.upToDate() {
		if err := w.cfg.Store.Sync(ctx, w.st); err != nil {
			return false, err
		}
	}

	r := w.st.Active()
	if r == nil {
		err := w.cfg.Queue.Release(ctx, w.lease, true)
		w.drop()
		if errors.Is(err, workqueue.ErrLeaseLost) {
			err = nil
		}
		return true, err
	}
	if !w.owns(r) {
		if r.Status == session.RunSuspended && r.SuspendReason != "" {
			if parked, err := w.reparkOverQuota(ctx); parked || err != nil {
				return true, err
			}
		}
		return true, w.startAttempt(ctx, r)
	}
	err := w.advance(ctx, r)
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

// upToDate 报告已知日志末尾没有超过本地投影，可以跳过本步的 Sync（压测中每个 Run 约 10 次读取）。
// 提示可能滞后：据此做出的决定若基于过期状态，追加时会冲突并重新同步，与没有提示时的竞争相同。
func (w *Worker) upToDate() bool {
	h, ok := w.cfg.Store.Log.(eventlog.HeadHinter)
	if !ok {
		return false
	}
	head, known := h.HeadHint(w.st.SessionID)
	return known && head <= w.st.Seq
}

func (w *Worker) owns(r *session.Run) bool {
	return r.ID == w.runID && r.Attempt == w.attempt && r.Status == session.RunRunning
}

// commit 以本 Worker 的 Attempt 追加事件；冲突时同步后若仍持有 Attempt 则重试。
// 若 Attempt 已失效（Run 被中断、接管或已终态），静默放弃。
func (w *Worker) commit(ctx context.Context, events ...*v1.Event) error {
	_, err := w.tryCommit(ctx, events...)
	return err
}

// tryCommit 同 commit，并报告事件是否已写入。
func (w *Worker) tryCommit(ctx context.Context, events ...*v1.Event) (bool, error) {
	for range 3 {
		err := w.cfg.Store.Commit(ctx, w.st, events...)
		if !errors.Is(err, eventlog.ErrConflict) {
			if err == nil {
				w.observeCommitted(events)
			}
			return err == nil, err
		}
		metrics.CommitConflicts.WithLabelValues().Inc()
		if err := w.cfg.Store.SyncAfterConflict(ctx, w.st); err != nil {
			if errors.Is(err, session.ErrGone) {
				// Session 已删除：放弃并丢弃全部本地状态。否则本地投影仍认为调用"已由我开始、尚无结果"，
				// 而按需续约又可能尚未发现租约已被移除，下一步会再次执行非幂等调用（模拟测试发现）。
				w.drop()
				return false, nil
			}
			return false, err
		}
		if r := w.st.Run(w.runID); r == nil || !w.owns(r) {
			return false, nil
		}
	}
	return false, fmt.Errorf("commit to session %s: too many conflicts", w.st.SessionID)
}

// suspend 追加 events 与 RunSuspended，并把 Session 停放到 until；之后本 Worker 不再持有该 Run。
func (w *Worker) suspend(ctx context.Context, r *session.Run, until time.Time, events ...*v1.Event) error {
	return w.suspendWith(ctx, &v1.RunSuspended{RunId: r.ID, Attempt: w.attempt}, until, events...)
}

func (w *Worker) suspendWith(ctx context.Context, s *v1.RunSuspended, until time.Time, events ...*v1.Event) error {
	events = append(events, &v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: s}})
	ok, err := w.tryCommit(ctx, events...)
	if err != nil || !ok {
		return err
	}
	err = w.cfg.Queue.Park(ctx, w.lease, until)
	w.drop()
	if errors.Is(err, workqueue.ErrLeaseLost) {
		return nil
	}
	return err
}

func (w *Worker) now() time.Time { return w.cfg.Store.Clock.Now() }

func (w *Worker) startAttempt(ctx context.Context, r *session.Run) error {
	if r.Status == session.RunRunning && r.StalledTakeovers >= w.cfg.MaxTakeovers {
		return w.fail(ctx, r, fmt.Sprintf("exceeded %d takeovers without progress", w.cfg.MaxTakeovers))
	}
	next := r.Attempt + 1
	// 提交前确定 Attempt 的类型：提交后投影中的状态已是 running。
	kind := map[session.RunStatus]string{session.RunQueued: "start", session.RunRunning: "takeover", session.RunSuspended: "resume"}[r.Status]
	err := w.cfg.Store.Commit(ctx, w.st, &v1.Event{Payload: &v1.Event_AttemptStarted{
		AttemptStarted: &v1.AttemptStarted{RunId: r.ID, Attempt: next, WorkerId: w.cfg.ID, LiveEndpoint: w.cfg.LiveEndpoint},
	}})
	if errors.Is(err, eventlog.ErrConflict) {
		return nil // 下一步 Sync 后重新决策
	}
	if err != nil {
		return err
	}
	w.runID, w.attempt, w.modelErrors, w.forceCompact = r.ID, next, 0, false
	metrics.Attempts.WithLabelValues(kind).Inc()
	w.cfg.Logger.Info("attempt started", "worker", w.cfg.ID, "session", w.st.SessionID, "run", r.ID, "attempt", next)
	return nil
}

func (w *Worker) fail(ctx context.Context, r *session.Run, reason string) error {
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	w.cfg.Logger.Warn("run failed", "session", w.st.SessionID, "run", r.ID, "reason", reason)
	e := &v1.Event{Payload: &v1.Event_RunFailed{RunFailed: &v1.RunFailed{RunId: r.ID, Attempt: r.Attempt, Reason: reason}}}
	err := w.cfg.Store.Commit(ctx, w.st, e)
	if errors.Is(err, eventlog.ErrConflict) {
		return nil
	}
	if err == nil {
		w.observeCommitted([]*v1.Event{e})
	}
	return err
}

func (w *Worker) advance(ctx context.Context, r *session.Run) error {
	def, err := w.cfg.Agents.Get(w.st.Agent)
	if err != nil {
		return w.fail(ctx, r, err.Error())
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
	return w.callModel(ctx, r, def)
}

func (w *Worker) toolResult(r *session.Run, callID string, content []*v1.ContentBlock, isErr bool) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{
		RunId: r.ID, Attempt: w.attempt, CallId: callID, Content: content, IsError: isErr,
	}}}
}

func (w *Worker) target() capability.Target {
	return capability.Target{
		Scope:     node.Scope{BusinessLine: w.st.Created.GetBusinessLine(), EndUser: w.st.Created.GetEndUser()},
		SessionID: w.st.SessionID,
	}
}

// advanceCall 推进一次 Capability 调用的一个阶段：
// 审批（m1 设计 §4）→ 进程内执行（m0 设计 §4）或路由派发（m1 设计 §3）。
func (w *Worker) advanceCall(ctx context.Context, r *session.Run, def *agentdef.Def, c *session.Call) error {
	id, name := c.Call.GetCallId(), c.Call.GetCapability()
	if c.Dispatched() {
		return w.awaitDispatched(ctx, r, c)
	}
	tool, err := w.cfg.Catalog.Resolve(ctx, w.target(), def.Capabilities, name)
	if err != nil {
		return err
	}
	if tool == nil {
		return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(fmt.Sprintf("capability %q is not available", name)), true))
	}

	// 需要审批：高风险能力；或 Memory 写入闸门判定（ADR-0023）；已经请求过审批的调用一律走完审批，
	// 即使之后的上下文变化使闸门不再触发。
	summary := approvalSummary(tool, c.Call)
	gateSummary, gated := memoryGate(w.st, c.Call)
	if gated {
		summary = gateSummary
	}
	if tool.RequiresApproval() || gated || c.Approval != nil {
		switch {
		case c.Approval == nil:
			deadline := w.now().Add(w.cfg.ApprovalTimeout)
			return w.suspend(ctx, r, deadline, &v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{
				RunId: r.ID, Attempt: w.attempt, CallId: id, Summary: summary, Deadline: timestamppb.New(deadline),
			}}})
		case c.AwaitingApproval() && !w.now().Before(c.Approval.Deadline):
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks("approval timed out: the user did not respond; the call was not executed"), true))
		case c.AwaitingApproval():
			return w.suspend(ctx, r, c.Approval.Deadline)
		case !c.Approval.Approved:
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks("denied: the user rejected this call; it was not executed"), true))
		}
	}

	if tool.Local == nil {
		deadline := w.now().Add(tool.Timeout)
		return w.commit(ctx, &v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{
			RunId: r.ID, Attempt: w.attempt, CallId: id, NodeId: tool.NodeID, Deadline: timestamppb.New(deadline),
		}}})
	}

	cp := tool.Local
	startedByMe := c.Started() && c.StartedAttempts[len(c.StartedAttempts)-1] == w.attempt
	if !startedByMe {
		if c.Started() && !cp.Spec().Idempotent {
			return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(
				"outcome unknown: the call was interrupted by a failure and may or may not have taken effect; it was not retried"), true))
		}
		return w.commit(ctx, &v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{
			RunId: r.ID, Attempt: w.attempt, CallId: id,
		}}})
	}

	var content []*v1.ContentBlock
	err = w.during(ctx, r, func(ctx context.Context) error {
		var err error
		content, err = cp.Invoke(ctx, capability.Invocation{
			SessionID: w.st.SessionID, RunID: r.ID, CallID: id, Arguments: c.Call.GetArgumentsJson(),
			BusinessLine: w.st.Created.GetBusinessLine(), EndUser: w.st.Created.GetEndUser(),
		})
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errSuperseded) {
			return nil
		}
		return w.commit(ctx, w.toolResult(r, id, model.TextBlocks(err.Error()), true))
	}
	return w.commit(ctx, w.toolResult(r, id, content, false))
}

// awaitDispatched 处理已派发的路由调用：超时则给出错误结果，否则（重新）投递并挂起等待。
// 重复投递是安全的：Node SDK 以 call_id 去重。
func (w *Worker) awaitDispatched(ctx context.Context, r *session.Run, c *session.Call) error {
	id := c.Call.GetCallId()
	if !w.now().Before(c.Deadline) {
		if err := w.cfg.Dispatch.Cancel(ctx, c.NodeID, id); err != nil {
			return err
		}
		return w.commit(ctx, w.toolResult(r, id, model.TextBlocks("timed out: the device did not return a result in time"), true))
	}
	_, capName, _ := strings.Cut(c.Call.GetCapability(), "__")
	err := w.cfg.Dispatch.Dispatch(ctx, c.NodeID, &v1.Invoke{
		SessionId: w.st.SessionID, RunId: r.ID, CallId: id, Capability: capName,
		ArgumentsJson: c.Call.GetArgumentsJson(), Deadline: timestamppb.New(c.Deadline),
	})
	if err != nil {
		return err
	}
	return w.suspend(ctx, r, c.Deadline)
}

func approvalSummary(t *capability.Tool, c *v1.ToolCall) string {
	where := "云端"
	if t.NodeID != "" && !strings.HasPrefix(t.Spec.Name, capability.SandboxLabel+"__") {
		where = "设备 " + strings.SplitN(t.Spec.Name, "__", 2)[0]
	}
	args, cut := model.Truncate(c.GetArgumentsJson(), 200)
	if cut {
		args += "…"
	}
	return fmt.Sprintf("在%s上执行 %s，参数 %s", where, t.Spec.Name, args)
}

func (w *Worker) callModel(ctx context.Context, r *session.Run, def *agentdef.Def) error {
	if p, err := w.overQuota(ctx, "run"); err != nil || p != nil {
		if err != nil {
			return err
		}
		return w.suspendWith(ctx, &v1.RunSuspended{RunId: r.ID, Attempt: w.attempt, Reason: p.Reason(), Until: timestamppb.New(p.ResetAt)},
			w.quotaPark(p))
	}
	tools, err := w.cfg.Catalog.Tools(ctx, w.target(), def.Capabilities)
	if err != nil {
		return w.fail(ctx, r, err.Error())
	}
	// 系统指令只含 AgentDef 的指令：召回放在 Run 的位置上（Transcript），使系统指令与工具定义构成的前缀
	// 在整个 Session 中不变，提供商的前缀缓存得以命中（docs/research/2026-10-agent-harness-and-system1.md）。
	req := &model.Request{Model: def.Model, System: def.Instructions, Messages: w.inlineImages(ctx, Transcript(w.st, def.Context.MaxToolResult))}
	for _, t := range tools {
		req.Tools = append(req.Tools, model.ToolSpec{Name: t.Spec.Name, Description: t.Spec.Description, InputSchema: t.Spec.InputSchema})
	}
	if through, usePrefix := planCompaction(w.st, req, def.Context, w.forceCompact); through > 0 {
		if !usePrefix {
			req = nil
		}
		return w.compact(ctx, r, def, through, req)
	}

	var resp *model.Response
	start := time.Now()
	defer func() { observeModel("turn", start, resp, err) }()
	err = w.during(ctx, r, func(ctx context.Context) error {
		var err error
		draft := live.Delta{SessionID: w.st.SessionID, RunID: r.ID, Attempt: w.attempt, AfterSeq: w.st.Seq}
		resp, err = w.cfg.Model.Generate(ctx, req, func(d model.Delta) {
			draft.Text = d.Text
			w.cfg.Live.Publish(draft)
		})
		draft.Text, draft.End = "", true
		w.cfg.Live.Publish(draft)
		return err
	})
	if err != nil {
		return w.modelFailed(ctx, r, "model", err)
	}
	w.modelErrors = 0
	// 在写日志之前计量：token 已经消耗，之后写日志失败（被接管、Session 被删除）也应计费（ADR-0019）。
	w.cfg.Meter.Model(ctx, w.usageScope(), "use_"+w.cfg.Store.IDs(), agentdef.Label(w.st.Agent), def.Model, resp.Usage, w.now())

	// 模型给出的调用 ID 可能为空或跨轮、跨 Run 重复；一律改写为全局唯一 ID，
	// 使其可作为下游幂等键。改写后的 ID 随事件落盘，后续上下文都使用它。
	for _, tc := range resp.ToolCalls {
		tc.CallId = "call_" + w.cfg.Store.IDs()
	}
	var events []*v1.Event
	// 内容安全（ADR-0021）：违规的回复替换为拒答文本、丢弃调用，原文不写入日志。服务不可用时不放行：
	// 返回错误使本步重试（下一步重新调用模型），持续不可用时由 StepErrorBudget 使 Run 失败。
	// 用单独的变量：err 被上面的 defer 用来记录模型调用的结果。
	v, merr := moderation.Check(ctx, w.cfg.Moderator, moderation.Request{Stage: moderation.Output,
		BusinessLine: w.st.Created.GetBusinessLine(), Text: ModerationText(resp.Content, resp.ToolCalls)})
	if merr != nil {
		return merr
	}
	if v.Block {
		w.cfg.Logger.Warn("model output blocked by moderation", "session", w.st.SessionID, "run", r.ID, "labels", v.Labels)
		events = append(events, &v1.Event{Payload: &v1.Event_ContentModerated{ContentModerated: &v1.ContentModerated{
			RunId: r.ID, Attempt: w.attempt, Stage: string(moderation.Output), Labels: v.Labels}}})
		resp.Content, resp.ToolCalls = model.TextBlocks(moderation.Refusal), nil
	}
	events = append(events, &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{
		RunId: r.ID, Attempt: w.attempt, Content: resp.Content, ToolCalls: resp.ToolCalls, Model: resp.Model, Usage: resp.Usage,
	}}})
	if len(resp.ToolCalls) == 0 {
		events = append(events, &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: r.ID, Attempt: w.attempt}}})
	}
	return w.commit(ctx, events...)
}

// inlineImageLimit 是内联给模型的单张图片上限。
const inlineImageLimit = 4 << 20

// inlineImages 把用户消息中的图片工件读出并内联；读取失败或过大时保留原引用（模型看到描述文本）。
func (w *Worker) inlineImages(ctx context.Context, msgs []model.Message) []model.Message {
	if w.cfg.Artifacts == nil {
		return msgs
	}
	for i, m := range msgs {
		if m.Role != model.RoleUser {
			continue
		}
		var blocks []*v1.ContentBlock
		for _, b := range m.Content {
			media := b.GetMedia()
			id, ok := artifact.ParseURI(media.GetUri())
			if !ok || !strings.HasPrefix(media.GetMimeType(), "image/") || media.GetSize() > inlineImageLimit {
				blocks = append(blocks, b)
				continue
			}
			data, err := w.readArtifact(ctx, id)
			if err != nil {
				w.cfg.Logger.Warn("inline image failed", "artifact", id, "err", err)
				blocks = append(blocks, b)
				continue
			}
			blocks = append(blocks, &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{
				MimeType: media.GetMimeType(), Name: media.GetName(), Size: media.GetSize(), Uri: media.GetUri(), Data: data,
			}}})
		}
		msgs[i].Content = blocks
	}
	return msgs
}

func (w *Worker) readArtifact(ctx context.Context, id string) ([]byte, error) {
	m, rc, err := w.cfg.Artifacts.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// 工件只对其所属 Session 可见。
	if m.SessionID != w.st.SessionID {
		return nil, artifact.ErrNotFound
	}
	return io.ReadAll(io.LimitReader(rc, inlineImageLimit))
}

// modelFailed 处理模型调用（含摘要）的失败：连续失败达到上限则 RunFailed，否则留待下一步重试。
// 上下文超长同样计入失败次数，并让下一步先压缩；压缩无法缓解时最终以 RunFailed 结束而不是无限重试。
func (w *Worker) modelFailed(ctx context.Context, r *session.Run, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errSuperseded) {
		return nil
	}
	if errors.Is(err, model.ErrContextOverflow) {
		w.forceCompact = true
	}
	w.modelErrors++
	if w.modelErrors >= w.cfg.MaxModelErrors {
		return w.fail(ctx, r, fmt.Sprintf("%s error: %v", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

func observeModel(kind string, start time.Time, resp *model.Response, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	metrics.ModelCallDuration.WithLabelValues(kind, outcome).Observe(metrics.Since(start))
	if resp != nil && resp.Usage != nil {
		metrics.ModelTokens.WithLabelValues("input").Add(float64(resp.Usage.GetInputTokens()))
		metrics.ModelTokens.WithLabelValues("output").Add(float64(resp.Usage.GetOutputTokens()))
		metrics.ModelTokens.WithLabelValues("cached_input").Add(float64(resp.Usage.GetCachedInputTokens()))
	}
}

// observeCommitted 为刚提交的事件记录指标：Run 终态（时长按事件时间计算）与上下文压缩。
func (w *Worker) observeCommitted(events []*v1.Event) {
	for _, e := range events {
		var runID, status string
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunCompleted:
			runID, status = p.RunCompleted.GetRunId(), "completed"
		case *v1.Event_RunFailed:
			runID, status = p.RunFailed.GetRunId(), "failed"
		case *v1.Event_ContextCompacted:
			metrics.Compactions.WithLabelValues().Inc()
			continue
		default:
			continue
		}
		agent := agentdef.Label(w.st.Agent)
		metrics.RunsFinished.WithLabelValues(status, agent).Inc()
		if r := w.st.Run(runID); r != nil {
			metrics.RunDuration.WithLabelValues(status, agent).Observe(e.GetTime().AsTime().Sub(r.RequestedAt).Seconds())
		}
	}
}

var errSuperseded = errors.New("attempt superseded")

// during 执行耗时操作 fn。期间若本 Attempt 被中断或接管，取消 fn 并返回 errSuperseded；
// 若配置了 Heartbeat，则期间持续续约，续约失败同样取消 fn。
func (w *Worker) during(ctx context.Context, r *session.Run, fn func(context.Context) error) error {
	opCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go w.watchSupersede(opCtx, cancel, w.st.SessionID, r.ID, w.st.Seq)
	if w.cfg.Heartbeat > 0 {
		go func() {
			t := time.NewTicker(w.cfg.Heartbeat)
			defer t.Stop()
			for {
				select {
				case <-opCtx.Done():
					return
				case <-t.C:
					if err := w.cfg.Queue.Renew(opCtx, w.lease, w.cfg.LeaseTTL); err != nil {
						cancel(errSuperseded)
						return
					}
				}
			}
		}()
	}

	err := fn(opCtx)
	if errors.Is(context.Cause(opCtx), errSuperseded) {
		return errSuperseded
	}
	return err
}

// watchSupersede 监视日志，一旦本 Run 终态或出现新的 Attempt 即取消当前操作。
func (w *Worker) watchSupersede(ctx context.Context, cancel context.CancelCauseFunc, sid, runID string, after uint64) {
	for {
		if _, err := w.cfg.Store.Log.Wait(ctx, sid, after); err != nil {
			return
		}
		events, err := eventlog.ReadAll(ctx, w.cfg.Store.Log, sid, after)
		if err != nil {
			return
		}
		for _, e := range events {
			after = e.GetSeq()
			switch p := e.GetPayload().(type) {
			case *v1.Event_RunInterrupted:
				if p.RunInterrupted.GetRunId() == runID {
					cancel(errSuperseded)
					return
				}
			case *v1.Event_AttemptStarted:
				if p.AttemptStarted.GetRunId() == runID {
					cancel(errSuperseded)
					return
				}
			}
		}
	}
}

// ModerationText 是送检的模型输出：回复文本加每个调用的能力名与参数。调用参数可能携带以用户名义发出的内容
// （消息、写入的文件、保存的 Memory），同样需要检查。
func ModerationText(content []*v1.ContentBlock, calls []*v1.ToolCall) string {
	var b strings.Builder
	b.WriteString(model.Text(content))
	for _, tc := range calls {
		fmt.Fprintf(&b, "\n%s %s", tc.GetCapability(), tc.GetArgumentsJson())
	}
	return b.String()
}
