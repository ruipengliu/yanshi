package runtime

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability/capname"
	"yanshi/internal/delegation"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// advanceDelegations 推进同一轮中相邻的委托调用（ADR-0055）：先逐个开始（一步一个 ToolCallStarted 或错误结果；
// 委托的风险低，不审批），都开始之后逐个检查超时，最后把仍在等待的全部（重新）派发，写一次 RunSuspended，
// 停放到其中最早的截止时间。之后子 Run 写回结果时唤醒父 Session，未全部完成就直接重新停放（Step）。
func (w *Worker) advanceDelegations(ctx context.Context, r *session.Run, def *agentdef.Def, front []*session.Call) error {
	var waiting []*session.Call
	for _, c := range front {
		switch {
		case c.Done:
		case !c.Dispatched():
			return w.advanceCall(ctx, r, def, c)
		default:
			waiting = append(waiting, c)
		}
	}
	for _, c := range waiting {
		// 与 awaitDispatched 相同：宽限期在截止时间之前结束就转入后台（ADR-0058），一步一个。
		if g := w.delegationGrace(c); !g.IsZero() && !w.now().Before(g) && g.Before(c.Deadline) {
			return w.backgroundDelegation(ctx, r, c)
		}
	}
	for _, c := range waiting {
		if !w.now().Before(c.Deadline) {
			if err := w.cfg.Dispatch.Cancel(ctx, w.st.Created.GetEndUser(), c.NodeID, c.Call.GetCallId()); err != nil {
				return err
			}
			return w.commit(ctx, w.toolResult(r, c.Call.GetCallId(),
				model.TextBlocks("timed out: the delegated agent did not finish in time; its task was interrupted"), true))
		}
	}
	until, _ := w.awaitingDelegations(r)
	_, err := w.redispatchAll(ctx, r, waiting, &v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: &v1.RunSuspended{
		RunId: r.ID, Attempt: w.attempt}}}, until)
	return err
}

// awaitingDelegations 报告 Run 是否只在等已派发、未到截止时间、也未到转入后台时刻的委托，返回其中最早的那个时刻
// （醒来时转入后台或给出超时结果）。挂起状态由调用方判断（Step 只对挂起的 Run 用它）。
func (w *Worker) awaitingDelegations(r *session.Run) (time.Time, bool) {
	now := w.now()
	var until time.Time
	for _, c := range r.DelegationFront() {
		if c.Done {
			continue
		}
		at := c.Deadline
		if g := w.delegationGrace(c); !g.IsZero() && g.Before(at) {
			at = g
		}
		switch {
		case !c.Dispatched() || !now.Before(at):
			return time.Time{}, false
		case until.IsZero() || at.Before(until):
			until = at
		}
	}
	return until, !until.IsZero()
}

// delegationGrace 返回委托调用转入后台的时刻（ADR-0058）：派发后经目标 AgentDef 的 delegation.background_after。
// 不是委托、本 Session 是委托派生的子 Session（它的委托 Run 产出回复，不能先以"已转后台"结束）、没有后台调用的了结
// 路径或目标关闭了后台（负数）时返回零值。目标的 AgentDef 按当前发布配置解析，与目录展示给模型的一致。
func (w *Worker) delegationGrace(c *session.Call) time.Time {
	if w.cfg.Background == nil || w.st.Created.GetParent() != nil || !delegation.IsNode(c.NodeID) || !c.Dispatched() {
		return time.Time{}
	}
	_, name, _ := capname.CutRouted(c.Call.GetCapability())
	def, err := w.cfg.Agents.Resolve(name, w.st.Created.GetEndUser())
	if err != nil || def.Delegation.BackgroundAfter <= 0 {
		return time.Time{}
	}
	return c.DispatchedAt.Add(def.Delegation.BackgroundAfter)
}

// backgroundDelegation 把超过宽限期的委托调用转入后台（ADR-0058）：写入占位结果，父 Run 照常继续；子 Session 继续执行，
// 它的回复之后作为一条后台结果输入送达（Delegator.Report）。截止时间不变，到期由 expireBackground 中断子 Run。
// 先按 call_id 幂等地重新派发，再写入：与设备调用相同（background），写入失败时撤回不再等待的投递。
func (w *Worker) backgroundDelegation(ctx context.Context, r *session.Run, c *session.Call) error {
	_, name, _ := capname.CutRouted(c.Call.GetCapability())
	res := w.toolResult(r, c.Call.GetCallId(), model.TextBlocks(DelegationPlaceholder(name)), false)
	res.GetToolResult().Backgrounded = &v1.Backgrounded{NodeId: c.NodeID, Deadline: timestamppb.New(c.Deadline)}
	if ok, err := w.redispatch(ctx, r, c, c.Deadline, res, time.Time{}); err != nil || !ok {
		return err
	}
	metrics.BackgroundCalls.WithLabelValues("delegation_backgrounded").Inc()
	w.cfg.Logger.Info("delegation backgrounded", "session", w.st.SessionID, "run", r.ID, "call", c.Call.GetCallId(), "node", c.NodeID)
	return nil
}

// DelegationPlaceholder 是委托转入后台时交给模型的结果（ADR-0058）：任务没有失败，仍在进行；结果稍后作为单独的消息送达。
// 明确不要重新委托：那会让另一个子 Session 再做一遍。
func DelegationPlaceholder(agent string) string {
	return fmt.Sprintf("in progress: agent %q is still working on this task, so you no longer wait for it in this turn. "+
		"Its result will arrive later as a separate message. Do not delegate this task again; tell the user it is in progress and continue.", agent)
}

// delegationRefused 检查一次委托能否派发：参数合法、委托链深度与同时未完成的委托数不超过委托方 AgentDef 的上限
// （docs/design/m5-multi-agent.md §4）。不能派发时返回作为错误结果交还模型的原因。
func (w *Worker) delegationRefused(r *session.Run, def *agentdef.Def, c *session.Call) string {
	if _, err := delegation.ParseArgs(c.Call.GetArgumentsJson()); err != nil {
		return err.Error()
	}
	// 本 Session 的深度是祖先数加一（顶层为 1），委托出去的子 Session 再深一层；上限含本层（agentdef.DelegationConfig）。
	if depth := len(w.st.Created.GetParent().GetAncestors()) + 2; depth > def.Delegation.MaxDepth {
		return fmt.Sprintf("cannot delegate: delegation depth %d exceeds the limit of %d; do the task yourself", depth, def.Delegation.MaxDepth)
	}
	// 按 Session 计数（ADR-0058）：本 Run 中已派发、尚无结果的，加上已转入后台、尚未了结的。
	open := 0
	for _, other := range r.Calls {
		if other.Dispatched() && delegation.IsNode(other.NodeID) {
			open++
		}
	}
	for _, b := range w.st.Background {
		if delegation.IsNode(b.NodeID) {
			open++
		}
	}
	if open >= def.Delegation.MaxFanout {
		return fmt.Sprintf("cannot delegate: %d delegated tasks are still running (limit %d)", open, def.Delegation.MaxFanout)
	}
	return ""
}
