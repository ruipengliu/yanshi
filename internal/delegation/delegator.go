package delegation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
	"yanshi/internal/workqueue"
)

// Dispatcher 派发与撤回路由调用（runtime.Dispatcher、service.Canceler）。
type Dispatcher interface {
	Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke, class workqueue.Class) error
	Cancel(ctx context.Context, nodeID, callID string) error
}

// Router 把委托目标交给 Delegator，其余交给 Next（设备与沙箱）。
type Router struct {
	Next      Dispatcher
	Delegator *Delegator
}

func (r *Router) Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke, class workqueue.Class) error {
	if IsNode(nodeID) {
		return r.Delegator.Dispatch(ctx, nodeID, inv)
	}
	return r.Next.Dispatch(ctx, nodeID, inv, class)
}

func (r *Router) Cancel(ctx context.Context, nodeID, callID string) error {
	if IsNode(nodeID) {
		return r.Delegator.Cancel(ctx, nodeID, callID)
	}
	return r.Next.Cancel(ctx, nodeID, callID)
}

// Delegator 派生子 Session、撤回委托，并把子 Run 的终态写回父调用。
type Delegator struct {
	Service *service.Service
	Logger  *slog.Logger
}

func (d *Delegator) log() *slog.Logger {
	if d.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Logger
}

// Dispatch 派生子 Session 并提交任务；重复调用幂等（子 Session ID 与输入 ID 都由调用推导）。父 Worker 每次从挂起
// 恢复都会再调用一次，与设备的重新投递相同。
func (d *Delegator) Dispatch(ctx context.Context, nodeID string, inv *v1.Invoke) error {
	child := ChildSession(nodeID)
	svc := d.Service
	// 子 Session 已被删除（ADR-0015）：不能因为重新派发而复活它，告诉父调用它没了。
	if gone, err := lifecycle.Deleted(ctx, svc.Deletions, child); err != nil || gone {
		if gone {
			return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), "the delegated session was deleted", true)
		}
		return err
	}
	// 父 Session 已删除（或正在删除）：没有人等这个结果，也不能再以它的 EndUser 派生新的 Session。
	if gone, err := lifecycle.Deleted(ctx, svc.Deletions, inv.GetSessionId()); err != nil || gone {
		return err
	}
	parent, err := svc.Store.Load(ctx, inv.GetSessionId())
	if err != nil {
		return err
	}
	if parent.Created == nil {
		return nil
	}
	args, err := ParseArgs(inv.GetArgumentsJson())
	if err != nil {
		return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), err.Error(), true)
	}
	ancestors := append([]*v1.AgentRef{parent.Agent}, parent.Created.GetParent().GetAncestors()...)
	_, err = svc.Create(ctx, service.CreateRequest{
		SessionID: child, BusinessLine: parent.Created.GetBusinessLine(), EndUser: parent.Created.GetEndUser(), Agent: inv.GetCapability(),
		Parent: &v1.ParentRef{SessionId: inv.GetSessionId(), RunId: inv.GetRunId(), CallId: inv.GetCallId(), Ancestors: ancestors},
	})
	if err != nil {
		if errors.Is(err, service.ErrInvalid) || errors.Is(err, service.ErrConflict) {
			return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), "cannot delegate: "+err.Error(), true)
		}
		return err
	}
	// 先写、后查（ADR-0015）：创建子 Session 的同时父 Session 被删除时，删除子 Session，不提交任务。
	if gone, err := lifecycle.Recheck(ctx, svc.Deletions, inv.GetSessionId(), func(ctx context.Context) error {
		return svc.Delete(ctx, child, "parent_deleted", "")
	}); err != nil || gone {
		return err
	}
	_, err = svc.SubmitDelegated(ctx, child, InputID(inv.GetCallId()), model.TextBlocks(args.Input()))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, service.ErrNotFound), errors.Is(err, service.ErrConflict), errors.Is(err, service.ErrInvalid),
		errors.Is(err, usage.ErrExceeded), errors.Is(err, moderation.ErrRejected):
		// 子 Session 已关闭或删除、输入不合法、配额用尽、任务被内容安全拒绝：任务不会执行，直接告诉父调用。
		return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), "cannot delegate: "+err.Error(), true)
	default:
		// 内容安全服务不可用、存储故障：返回错误，父 Worker 下一步重试；持续失败由父调用的时限兜底。
		return err
	}
}

// Cancel 中断子 Session 中由这次委托开始的 Run（父 Run 被中断或委托超时）。子 Run 的中断照常级联到它自己的委托。
func (d *Delegator) Cancel(ctx context.Context, nodeID, callID string) error {
	child := ChildSession(nodeID)
	st, err := d.Service.Store.Load(ctx, child)
	if err != nil || st.Created == nil {
		return err
	}
	ref := st.Input(InputID(callID))
	if ref == nil {
		return nil
	}
	err = d.Service.Interrupt(ctx, child, ref.RunID)
	if errors.Is(err, service.ErrNotFound) {
		return nil
	}
	return err
}

// Report 把子 Session 中委托 Run 的终态写回父调用（外部结果，与父 Session 的唤醒同一事务）。子 Worker 在子 Session
// 没有活跃 Run 时调用；重复调用无害：父调用已有结果、父 Run 已结束或父 Session 已删除时结果作废。
func (d *Delegator) Report(ctx context.Context, child *session.State) error {
	p := child.Created.GetParent()
	if p == nil {
		return nil
	}
	ref := child.Input(InputID(p.GetCallId()))
	if ref == nil {
		return nil // 任务尚未提交（派发时会提交）
	}
	sum, ok := child.RunSummary(ref.RunID)
	if !ok || !sum.Status.Terminal() {
		return nil
	}
	text, err := d.lastMessage(ctx, child, ref.RunID)
	if err != nil {
		return err
	}
	switch sum.Status {
	case session.RunCompleted:
		return d.result(ctx, p.GetSessionId(), p.GetRunId(), p.GetCallId(), text, false)
	case session.RunFailed:
		return d.result(ctx, p.GetSessionId(), p.GetRunId(), p.GetCallId(), "the delegated agent failed: "+text, true)
	default:
		return d.result(ctx, p.GetSessionId(), p.GetRunId(), p.GetCallId(), "the delegated task was interrupted", true)
	}
}

// lastMessage 是子 Run 最后一条模型回复的文字（Run 可能已移出投影，读日志）。
func (d *Delegator) lastMessage(ctx context.Context, child *session.State, runID string) (string, error) {
	text := ""
	err := session.ScanLog(ctx, d.Service.Store.Log, child.SessionID, func(e *v1.Event) bool {
		if m := e.GetAssistantMessage(); m.GetRunId() == runID {
			if t := model.Text(m.GetContent()); t != "" {
				text = t
			}
		}
		if f := e.GetRunFailed(); f.GetRunId() == runID {
			text = f.GetReason()
		}
		return e.GetSeq() < child.Seq
	})
	if text == "" {
		text = "(no reply)"
	}
	return text, err
}

// result 写入父调用的外部结果并唤醒父 Session；调用已有结果、Run 已终态或 Session 已删除时静默作废。
func (d *Delegator) result(ctx context.Context, sessionID, runID, callID, text string, isErr bool) error {
	store := d.Service.Store
	st, err := store.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	for range 16 {
		err := store.CommitAndWake(ctx, st, &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{
			RunId: runID, CallId: callID, Content: model.TextBlocks(text), IsError: isErr}}})
		switch {
		case err == nil:
			return nil
		case errors.Is(err, session.ErrInvalid):
			d.log().Info("delegation result dropped", "session", sessionID, "call", callID, "reason", err)
			return nil
		case errors.Is(err, eventlog.ErrConflict):
			if err := store.SyncAfterConflict(ctx, st); err != nil {
				if errors.Is(err, session.ErrGone) {
					return nil
				}
				return err
			}
		default:
			return err
		}
	}
	return fmt.Errorf("delegation result for call %s: too many conflicts", callID)
}

// Directory 是可委托的 AgentDef（capability.AgentSource），取自 AgentDef 注册表与发布配置。
type Directory struct{ Registry *agentdef.Registry }

func (d Directory) Agents(endUser string) []capability.AgentInfo {
	seen := map[string]bool{}
	var out []capability.AgentInfo
	for _, def := range d.Registry.All() {
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		cur, err := d.Registry.Resolve(def.Name, endUser)
		if err != nil {
			continue
		}
		out = append(out, capability.AgentInfo{Name: cur.Name, Description: cur.Description, Timeout: cur.Delegation.Timeout})
	}
	return out
}
