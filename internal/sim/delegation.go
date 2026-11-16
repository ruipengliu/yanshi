package sim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/decision"
	"yanshi/internal/delegation"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
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
//   - 收敛后：同一个 Run 同时已派发的委托从未超过扇出上限（checkFanout）；
//   - 收敛后：父 Session 删除完成，则子 Session 也已删除完成。
func (w *World) checkDelegation(final bool) error {
	ctx := context.Background()
	if final {
		for _, sid := range append(slices.Clone(w.sessions), w.children...) {
			events, err := eventlog.ReadAll(ctx, w.log, sid, 0)
			if err != nil {
				return err
			}
			if _, err := checkFanout(sid, events); err != nil {
				return err
			}
			if _, _, err := w.checkArtifactCopies(sid, events); err != nil {
				return err
			}
		}
	}
	for _, child := range w.children {
		st, err := w.store.Load(ctx, child)
		if err != nil {
			return err
		}
		if st.Created != nil {
			p := st.Created.GetParent()
			expected, err := delegation.ChildSessionID(p.GetSessionId(), p.GetCallId(), st.Created.GetEndUser())
			if err != nil || p.GetSessionId() != w.parentOf[child] || expected != child {
				return fmt.Errorf("invariant: child session %s has parent %v, discovered under %s", child, p, w.parentOf[child])
			}
			parent, err := w.store.Load(ctx, p.GetSessionId())
			if err != nil {
				return err
			}
			if parent.Created != nil && (parent.Created.GetEndUser() != st.Created.GetEndUser() || parent.Created.GetBusinessLine() != st.Created.GetBusinessLine()) {
				return fmt.Errorf("invariant: child session %s changed parent ownership", child)
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
		// 交来的与子 Run 自己产出的工件随子 Session 一并删除。
		if list, _ := w.artifacts.List(ctx, child); len(list) > 0 {
			return fmt.Errorf("invariant: deleted child session %s has %d artifacts", child, len(list))
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

func (f flakyCancel) Cancel(ctx context.Context, endUser, nodeID, callID string) error {
	if delegation.IsNode(nodeID) && f.w.faults && f.w.chance(0.3) {
		f.w.Stats.CascadeFaults++
		return cancelFault{}
	}
	return f.Router.Cancel(ctx, endUser, nodeID, callID)
}

// simMaxDepth 是 sim Agent 的委托链深度上限（AgentDef 默认值）。
const simMaxDepth = 2

// simMaxFanout 是 sim Agent 的一个 Run 同时未完成的委托数上限。
const simMaxFanout = 2

// checkFanout 检查委托扇出的不变量：任何时刻同一个 Run 已派发、尚无结果的委托不超过上限（ADR-0055）。
// 返回同时在等两个以上委托的 Run 数，计入覆盖统计。
func checkFanout(sid string, events []*v1.Event) (parallel int, err error) {
	open := map[string]map[string]bool{}
	counted := map[string]bool{}
	for _, e := range events {
		switch {
		case e.GetToolCallStarted() != nil && delegation.IsNode(e.GetToolCallStarted().GetNodeId()):
			s := e.GetToolCallStarted()
			if open[s.GetRunId()] == nil {
				open[s.GetRunId()] = map[string]bool{}
			}
			open[s.GetRunId()][s.GetCallId()] = true
			n := len(open[s.GetRunId()])
			if n > simMaxFanout {
				return parallel, fmt.Errorf("invariant: session %s run %s has %d delegations in flight, limit %d", sid, s.GetRunId(), n, simMaxFanout)
			}
			if n >= 2 && !counted[s.GetRunId()] {
				counted[s.GetRunId()] = true
				parallel++
			}
		case e.GetToolResult() != nil:
			delete(open[e.GetToolResult().GetRunId()], e.GetToolResult().GetCallId())
		}
	}
	return parallel, nil
}

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

// checkArtifactCopies 检查委托交接工件的不变量（ADR-0055），返回本 Session 中父 → 子与子 → 父的复制数：
//   - 每条复制记录连接的是由同一次委托调用派生的父子：父 → 子的来源是父 Session，子 → 父的来源是由该调用派生的
//     子 Session；不扩大可见范围，别的 Session 的工件进不来；
//   - 父 → 子只复制父 Run 在这次委托的参数中点名的工件；
//   - Session 没有删除记录时，副本作为本 Session 的工件存在。
func (w *World) checkArtifactCopies(sid string, events []*v1.Event) (handoffs, handbacks int, err error) {
	ctx := context.Background()
	if len(events) == 0 {
		return 0, 0, nil
	}
	created := events[0].GetSessionCreated()
	endUser := created.GetEndUser()
	for _, e := range events {
		c := e.GetArtifactCopied()
		if c == nil {
			continue
		}
		if p := created.GetParent(); p != nil && c.GetSourceSessionId() == p.GetSessionId() {
			if c.GetCallId() != p.GetCallId() {
				return 0, 0, fmt.Errorf("invariant: session %s received artifact %s for call %s, delegated by call %s", sid, c.GetArtifactId(), c.GetCallId(), p.GetCallId())
			}
			if named, ok, err := w.namedArtifacts(p.GetSessionId(), c.GetCallId()); err != nil {
				return 0, 0, err
			} else if ok && !slices.Contains(named, c.GetSourceArtifactId()) {
				return 0, 0, fmt.Errorf("invariant: session %s received parent artifact %s that call %s did not name (%v)", sid, c.GetSourceArtifactId(), c.GetCallId(), named)
			}
			handoffs++
		} else {
			child, err := delegation.ChildSessionID(sid, c.GetCallId(), endUser)
			if err != nil || child != c.GetSourceSessionId() {
				return 0, 0, fmt.Errorf("invariant: session %s copied artifact %s from %s, not from the child of call %s", sid, c.GetSourceArtifactId(), c.GetSourceSessionId(), c.GetCallId())
			}
			handbacks++
		}
		if _, err := w.deletions.Get(ctx, sid); err == nil {
			continue // 删除中：工件可能已先于日志删除
		}
		if _, err := w.artifacts.Stat(ctx, sid, c.GetArtifactId()); err != nil {
			return 0, 0, fmt.Errorf("invariant: session %s lost copied artifact %s: %v", sid, c.GetArtifactId(), err)
		}
	}
	return handoffs, handbacks, nil
}

// namedArtifacts 返回父 Session 中委托调用 callID 的参数点名的工件；父日志已删除时 ok 为 false。
func (w *World) namedArtifacts(parent, callID string) ([]string, bool, error) {
	events, err := eventlog.ReadAll(context.Background(), w.log, parent, 0)
	if err != nil || len(events) == 0 {
		return nil, false, err
	}
	for _, e := range events {
		for _, tc := range e.GetAssistantMessage().GetToolCalls() {
			if tc.GetCallId() != callID {
				continue
			}
			args, err := delegation.ParseArgs(tc.GetArgumentsJson())
			if err != nil {
				return nil, true, nil
			}
			return args.Artifacts, true, nil
		}
	}
	return nil, true, nil
}

// simArtifacts 返回模型看到的消息中引用的工件 ID（用户上传、委托交来或交回的），按出现顺序。
func simArtifacts(messages []model.Message) []string {
	var out []string
	for _, m := range messages {
		for _, b := range m.Content {
			if id, ok := artifact.ParseURI(b.GetMedia().GetUri()); ok && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}
