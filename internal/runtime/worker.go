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
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

// Dispatcher 把路由调用投递到 Node 的 Inbox（由 node.Hub 实现）。
type Dispatcher interface {
	Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke) error
	Cancel(ctx context.Context, nodeID, callID string) error
}

type Config struct {
	ID       string
	Store    *session.Store
	Queue    workqueue.Queue
	Agents   *agentdef.Registry
	Model    model.Provider
	Catalog  *capability.Catalog
	Dispatch Dispatcher
	// Artifacts 非空时，用户消息中的图片工件会内联给模型（docs/design/m2-artifacts.md §4）。
	Artifacts *artifact.Service
	Live      live.Bus
	Logger    *slog.Logger

	LeaseTTL time.Duration
	// Heartbeat > 0 时，在模型调用与 Capability 执行期间按此间隔续约（真实时间）。
	// 模拟测试置 0。
	Heartbeat time.Duration
	// MaxTakeovers 是一个 Run 最多被接管的次数，超过则 RunFailed。从挂起恢复不计入。
	MaxTakeovers int
	// ApprovalTimeout 是审批的等待时长。
	ApprovalTimeout time.Duration
	// MaxModelErrors 是同一 Attempt 内连续模型错误的上限，超过则 RunFailed。
	MaxModelErrors int
	// IdleWait 是 Run 循环在无事可做时的等待时间。
	IdleWait time.Duration
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
	if c.Live == nil {
		c.Live = live.Discard{}
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
	runID       string
	attempt     uint32
	modelErrors int
}

func New(cfg Config) *Worker {
	cfg.defaults()
	return &Worker{cfg: cfg}
}

func (w *Worker) ID() string { return w.cfg.ID }

// Run 持续执行 Step 直到 ctx 结束。
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
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
	w.lease, w.st, w.runID, w.attempt, w.modelErrors = nil, nil, "", 0, 0
}

// Step 推进一件工作；返回 false 表示当前无事可做。
func (w *Worker) Step(ctx context.Context) (bool, error) {
	if w.lease == nil {
		l, err := w.cfg.Queue.Claim(ctx, w.cfg.ID, w.cfg.LeaseTTL)
		if errors.Is(err, workqueue.ErrEmpty) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		w.drop()
		w.lease = l
	} else if err := w.cfg.Queue.Renew(ctx, w.lease, w.cfg.LeaseTTL); err != nil {
		w.drop()
		if errors.Is(err, workqueue.ErrLeaseLost) {
			return true, nil
		}
		return false, err
	}

	if w.st == nil {
		st, err := w.cfg.Store.Load(ctx, w.lease.SessionID)
		if err != nil {
			return false, err
		}
		w.st = st
	} else if err := w.cfg.Store.Sync(ctx, w.st); err != nil {
		return false, err
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
		return true, w.startAttempt(ctx, r)
	}
	return true, w.advance(ctx, r)
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
			return err == nil, err
		}
		if err := w.cfg.Store.Sync(ctx, w.st); err != nil {
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
	events = append(events, &v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: &v1.RunSuspended{RunId: r.ID, Attempt: w.attempt}}})
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
	if r.Status == session.RunRunning && r.Takeovers >= w.cfg.MaxTakeovers {
		return w.fail(ctx, r, fmt.Sprintf("exceeded %d takeovers", w.cfg.MaxTakeovers))
	}
	next := r.Attempt + 1
	err := w.cfg.Store.Commit(ctx, w.st, &v1.Event{Payload: &v1.Event_AttemptStarted{
		AttemptStarted: &v1.AttemptStarted{RunId: r.ID, Attempt: next, WorkerId: w.cfg.ID},
	}})
	if errors.Is(err, eventlog.ErrConflict) {
		return nil // 下一步 Sync 后重新决策
	}
	if err != nil {
		return err
	}
	w.runID, w.attempt, w.modelErrors = r.ID, next, 0
	w.cfg.Logger.Info("attempt started", "worker", w.cfg.ID, "session", w.st.SessionID, "run", r.ID, "attempt", next)
	return nil
}

func (w *Worker) fail(ctx context.Context, r *session.Run, reason string) error {
	w.cfg.Logger.Warn("run failed", "session", w.st.SessionID, "run", r.ID, "reason", reason)
	err := w.cfg.Store.Commit(ctx, w.st, &v1.Event{Payload: &v1.Event_RunFailed{
		RunFailed: &v1.RunFailed{RunId: r.ID, Attempt: r.Attempt, Reason: reason},
	}})
	if errors.Is(err, eventlog.ErrConflict) {
		return nil
	}
	return err
}

func (w *Worker) advance(ctx context.Context, r *session.Run) error {
	def, err := w.cfg.Agents.Get(w.st.Created.GetAgent())
	if err != nil {
		return w.fail(ctx, r, err.Error())
	}
	if c := r.PendingCall(); c != nil {
		return w.advanceCall(ctx, r, def, c)
	}
	if r.Turns >= def.MaxTurns {
		return w.fail(ctx, r, fmt.Sprintf("exceeded %d turns", def.MaxTurns))
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

	if tool.RequiresApproval() {
		switch {
		case c.Approval == nil:
			deadline := w.now().Add(w.cfg.ApprovalTimeout)
			return w.suspend(ctx, r, deadline, &v1.Event{Payload: &v1.Event_ApprovalRequested{ApprovalRequested: &v1.ApprovalRequested{
				RunId: r.ID, Attempt: w.attempt, CallId: id, Summary: approvalSummary(tool, c.Call), Deadline: timestamppb.New(deadline),
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
	args := c.GetArgumentsJson()
	if len(args) > 200 {
		args = args[:200] + "…"
	}
	return fmt.Sprintf("在%s上执行 %s，参数 %s", where, t.Spec.Name, args)
}

func (w *Worker) callModel(ctx context.Context, r *session.Run, def *agentdef.Def) error {
	tools, err := w.cfg.Catalog.Tools(ctx, w.target(), def.Capabilities)
	if err != nil {
		return w.fail(ctx, r, err.Error())
	}
	req := &model.Request{Model: def.Model, System: def.Instructions, Messages: w.inlineImages(ctx, Transcript(w.st))}
	for _, t := range tools {
		req.Tools = append(req.Tools, model.ToolSpec{Name: t.Spec.Name, Description: t.Spec.Description, InputSchema: t.Spec.InputSchema})
	}

	var resp *model.Response
	err = w.during(ctx, r, func(ctx context.Context) error {
		var err error
		resp, err = w.cfg.Model.Generate(ctx, req, func(d model.Delta) {
			w.cfg.Live.Publish(live.Delta{SessionID: w.st.SessionID, RunID: r.ID, Attempt: w.attempt, Text: d.Text})
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
		w.modelErrors++
		if w.modelErrors >= w.cfg.MaxModelErrors {
			return w.fail(ctx, r, fmt.Sprintf("model error: %v", err))
		}
		return fmt.Errorf("model: %w", err)
	}
	w.modelErrors = 0

	// 模型给出的调用 ID 可能为空或跨轮、跨 Run 重复；一律改写为全局唯一 ID，
	// 使其可作为下游幂等键。改写后的 ID 随事件落盘，后续上下文都使用它。
	for _, tc := range resp.ToolCalls {
		tc.CallId = "call_" + w.cfg.Store.IDs()
	}
	events := []*v1.Event{{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{
		RunId: r.ID, Attempt: w.attempt, Content: resp.Content, ToolCalls: resp.ToolCalls, Model: resp.Model, Usage: resp.Usage,
	}}}}
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
