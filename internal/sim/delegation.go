package sim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/decision"
	"yanshi/internal/delegation"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/service"
)

// 委托的模拟（docs/design/m5-multi-agent.md §6）：sim Agent 可以委托给自己（agent__sim），子 Session 中的 Run 照常
// 经历故障、审批、提问、设备调用与再委托。子 Session 由父日志中的委托目标发现，审批、中断、收敛与删除检查都覆盖它们。

// discoverChildren 找出已知 Session 委托派生的子 Session（包括子的子）。
func (w *World) discoverChildren() error {
	ctx := context.Background()
	for i := 0; i < len(w.known()); i++ {
		sid := w.known()[i]
		events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
		if err != nil {
			return err
		}
		for _, e := range events {
			id := e.GetToolCallStarted().GetNodeId()
			if !delegation.IsNode(id) {
				continue
			}
			child := delegation.ChildSession(id)
			if _, ok := w.parentOf[child]; !ok {
				w.parentOf[child] = sid
				w.children = append(w.children, child)
			}
		}
	}
	return nil
}

// known 是模拟中出现过的全部 Session：当前的、已关闭的与子 Session。
func (w *World) known() []string {
	return append(append(slices.Clone(w.sessions), w.closed...), w.children...)
}

// live 是可能还有进行中 Run 的 Session：当前的 Session 与已创建、未删除的子 Session。
func (w *World) live() []string {
	out := slices.Clone(w.sessions)
	for _, c := range w.children {
		if st, err := w.store.Load(context.Background(), c); err == nil && st.Created != nil {
			if gone, _ := lifecycle.Deleted(context.Background(), w.deletions, c); !gone {
				out = append(out, c)
			}
		}
	}
	return out
}

// checkDelegation 检查委托的不变量：
//   - 子 Session 的来处与父日志一致，委托链深度不超过上限；
//   - 收敛后：父 Session 删除完成，则子 Session 也已删除完成。
func (w *World) checkDelegation(final bool) error {
	ctx := context.Background()
	for _, child := range w.children {
		st, err := w.store.Load(ctx, child)
		if err != nil {
			return err
		}
		if st.Created != nil {
			p := st.Created.GetParent()
			if p.GetSessionId() != w.parentOf[child] || delegation.ChildSessionID(p.GetSessionId(), p.GetCallId()) != child {
				return fmt.Errorf("invariant: child session %s has parent %v, discovered under %s", child, p, w.parentOf[child])
			}
			if depth := len(p.GetAncestors()) + 1; depth > simMaxDepth {
				return fmt.Errorf("invariant: child session %s is at delegation depth %d, limit %d", child, depth, simMaxDepth)
			}
		}
		if !final {
			continue
		}
		parent, err := w.deletions.Get(ctx, w.parentOf[child])
		if err != nil || parent.CompletedAt.IsZero() {
			continue
		}
		t, err := w.deletions.Get(ctx, child)
		if err != nil || t.CompletedAt.IsZero() {
			if st.Created == nil {
				continue // 父 Run 结束前没来得及派发：子 Session 从未创建
			}
			return fmt.Errorf("invariant: child session %s survived the deletion of its parent %s", child, w.parentOf[child])
		}
	}
	return nil
}

// countDelegationResults 统计父日志中由子 Run 写回的委托结果（不含超时与派发前的拒绝）。
func (w *World) countDelegationResults(events []*v1.Event) {
	agentCalls := map[string]bool{}
	for _, e := range events {
		if id := e.GetToolCallStarted().GetNodeId(); delegation.IsNode(id) {
			agentCalls[e.GetToolCallStarted().GetCallId()] = true
		}
		if r := e.GetToolResult(); r.GetAttempt() == 0 && agentCalls[r.GetCallId()] {
			w.Stats.DelegationResults++
		}
	}
}

// deleteChildMidDispatch 在派发检查过子 Session 的删除记录之后、创建它之前，模拟用户删除这个子 Session 且 Janitor
// 清理完毕（重新派发时子 Session 已存在）。派发若不对子 Session 本身先写后查，会重建它的索引与日志，
// 删除不变量（checkDeleted）随即报告复活。
func (w *World) deleteChildMidDispatch(ctx context.Context, child string) {
	// 重新派发本身就少见，概率低时 300 个种子中只有几次完成这一交错。
	if !w.faults || !w.chance(0.6) {
		return
	}
	if st, err := w.store.Load(ctx, child); err != nil || st.Created == nil {
		return
	}
	if gone, err := lifecycle.Deleted(ctx, w.deletions, child); err != nil || gone {
		return
	}
	if err := w.svc.Delete(ctx, child, "end_user", ""); err != nil {
		return
	}
	w.deleted = append(w.deleted, child)
	w.tracef("delete child %s (mid-dispatch)", child)
	// 专用的 Janitor 不注入崩溃：这里要的是"清理已完成"这一交错。清理队列中可能还有别的 Session，逐个处理完。
	j := w.newJanitor()
	j.BeforeStage = nil
	for range 64 {
		if t, err := w.deletions.Get(ctx, child); err == nil && !t.CompletedAt.IsZero() {
			w.Stats.ChildDeletionRaces++
			return
		}
		if did, err := j.Step(context.WithoutCancel(ctx)); err != nil || !did {
			return
		}
	}
}

// cancelFault 是注入的撤回失败（存储故障）。
type cancelFault struct{}

func (cancelFault) Error() string { return "simulated failure withdrawing a delegated task" }

// flakyCancel 是 Service 撤回路由调用所用的 Dispatcher：撤回委托偶尔在生效之前失败。中断父 Run 时撤回失败，子 Run
// 就成了没人等的孤儿，须由子 Worker 自己发现并停下（Delegator.Reconcile）。
type flakyCancel struct {
	*delegation.Router
	w *World
}

func (f flakyCancel) Cancel(ctx context.Context, nodeID, callID string) error {
	if delegation.IsNode(nodeID) && f.w.faults && f.w.chance(0.3) {
		f.w.Stats.CascadeFaults++
		return cancelFault{}
	}
	return f.Router.Cancel(ctx, nodeID, callID)
}

// simMaxDepth 是 sim Agent 的委托链深度上限（AgentDef 默认值）。
const simMaxDepth = 2

// simDecider 是模拟的判断器（docs/design/m6-decisions.md）：按随机源给出选项，故障注入时可能失败或让 Worker 崩溃。
type simDecider struct{ w *World }

func (d simDecider) Decide(ctx context.Context, q decision.Question) (decision.Answer, error) {
	w := d.w
	if w.faults {
		switch {
		case w.chance(0.1):
			return decision.Answer{}, errors.New("sim: decider unavailable")
		case w.chance(0.02):
			w.crash()
			return decision.Answer{}, ctx.Err()
		}
	}
	return decision.Answer{Chosen: q.Options[w.rng.IntN(len(q.Options))], Confidence: 0.8, Model: "sim/decider"}, nil
}

// redispatchDelegation 模拟父 Worker 从挂起恢复时重新派发等待中的委托（Delegator.Dispatch 幂等，父 Worker 每次恢复都会再
// 派发一次）。重新派发本来只在父 Run 恰好被别的原因唤醒时发生，"重新派发时子 Session 被删除"（deleteChildMidDispatch）
// 在 300 个种子中只有几次，任何改变随机序列的改动都可能使它归零；这一动作使它稳定出现。
func (w *World) redispatchDelegation() error {
	ctx := context.Background()
	live := w.live()
	off := w.rng.IntN(len(live))
	for i := range live {
		sid := live[(off+i)%len(live)]
		st, err := w.store.Load(ctx, sid)
		if err != nil || st.Created == nil {
			continue
		}
		a := st.Active()
		if a == nil {
			continue
		}
		for _, c := range a.Calls {
			if c.Done || !c.Dispatched() || !delegation.IsNode(c.NodeID) {
				continue
			}
			_, agent, _ := strings.Cut(c.Call.GetCapability(), "__")
			err := w.delegator.Dispatch(ctx, c.NodeID, &v1.Invoke{SessionId: sid, RunId: a.ID, CallId: c.Call.GetCallId(),
				Capability: agent, ArgumentsJson: c.Call.GetArgumentsJson(), Deadline: timestamppb.New(c.Deadline)})
			w.tracef("redispatch %s %s: %v", sid, c.Call.GetCallId(), err)
			var sf *storeFault
			if err == nil || errors.As(err, &sf) || w.submitModerated(err) || w.submitOverQuota(err) || errors.Is(err, service.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("redispatch %s: %w", c.Call.GetCallId(), err)
		}
	}
	return nil
}
