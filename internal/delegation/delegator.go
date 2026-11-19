package delegation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
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
	Dispatch(ctx context.Context, endUser, nodeID string, inv *v1.Invoke, class workqueue.Class) error
	Cancel(ctx context.Context, endUser, nodeID, callID string) error
}

// Router 把委托目标交给 Delegator，其余交给 Next（设备与沙箱）。
type Router struct {
	Next      Dispatcher
	Delegator *Delegator
}

func (r *Router) Dispatch(ctx context.Context, endUser, nodeID string, inv *v1.Invoke, class workqueue.Class) error {
	if IsNode(nodeID) {
		return r.Delegator.Dispatch(ctx, nodeID, inv)
	}
	return r.Next.Dispatch(ctx, endUser, nodeID, inv, class)
}

func (r *Router) Cancel(ctx context.Context, endUser, nodeID, callID string) error {
	if IsNode(nodeID) {
		return r.Delegator.Cancel(ctx, nodeID, callID)
	}
	return r.Next.Cancel(ctx, endUser, nodeID, callID)
}

// Delegator 派生子 Session、撤回委托，并把子 Run 的终态写回父调用。
type Delegator struct {
	Service *service.Service
	// Artifacts 复制委托交接的工件（ADR-0055）；nil 时委托不能带工件，子 Run 的回复也不交回工件。
	Artifacts *artifact.Service
	Logger    *slog.Logger
	// BeforeCreate 非 nil 时在创建子 Session 之前调用：模拟测试在这里注入"子 Session 恰好此时被删除"。
	BeforeCreate func(ctx context.Context, child string)
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
	// 归属只取父投影；不能把调用方传来的目标或槽位当作真实 EndUser。
	expected, err := ChildSessionID(parent.SessionID, inv.GetCallId(), parent.Created.GetEndUser())
	if err != nil {
		return err
	}
	if nodeID != NodeID(expected) {
		return fmt.Errorf("%w: delegation target does not match parent", service.ErrInvalid)
	}
	args, err := ParseArgs(inv.GetArgumentsJson())
	if err != nil {
		return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), err.Error(), true)
	}
	// 点名的工件须是父 Session 的：在创建子 Session 之前检查，引用错了不留下空的子 Session。
	if msg, err := d.checkArtifacts(ctx, parent.SessionID, args.Artifacts); err != nil || msg != "" {
		if msg != "" {
			return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), msg, true)
		}
		return err
	}
	ancestors := append([]*v1.AgentRef{parent.Agent}, parent.Created.GetParent().GetAncestors()...)
	if d.BeforeCreate != nil {
		d.BeforeCreate(ctx, child)
	}
	var deleteAt time.Time
	if at := parent.Created.GetDeleteAt(); at != nil {
		deleteAt = at.AsTime()
	}
	_, err = svc.Create(ctx, service.CreateRequest{
		DeleteAt:  deleteAt,
		TimeZone:  parent.Created.GetTimeZone(),
		SessionID: child, BusinessLine: parent.Created.GetBusinessLine(), EndUser: parent.Created.GetEndUser(), Agent: inv.GetCapability(),
		Parent: &v1.ParentRef{SessionId: inv.GetSessionId(), RunId: inv.GetRunId(), CallId: inv.GetCallId(), Ancestors: ancestors},
	})
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), "the delegated session was deleted", true)
		}
		if errors.Is(err, service.ErrInvalid) || errors.Is(err, service.ErrConflict) {
			return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), "cannot delegate: "+err.Error(), true)
		}
		return err
	}
	// 先写、后查（ADR-0015），对子 Session 本身：子 Session ID 由调用推导，开头的检查之后、创建之前它可能被删除
	// 并清理完毕，创建会重建它的索引与日志。撤回之后告诉父调用它没了。
	if gone, err := lifecycle.Recheck(ctx, svc.Deletions, child, func(ctx context.Context) error {
		return d.withdrawChild(ctx, parent.Created.GetEndUser(), child)
	}); err != nil || gone {
		if gone {
			return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), "the delegated session was deleted", true)
		}
		return err
	}
	// 同样对父 Session：创建子 Session 的同时父 Session 被删除时，删除子 Session，不提交任务。
	if gone, err := lifecycle.Recheck(ctx, svc.Deletions, inv.GetSessionId(), func(ctx context.Context) error {
		t, err := svc.Deletions.Get(ctx, inv.GetSessionId())
		if err != nil {
			return err
		}
		reason := "parent_deleted"
		if lifecycle.KeepsMemory(t.Reason) {
			reason = t.Reason
		}
		return svc.Delete(ctx, child, reason, "")
	}); err != nil || gone {
		return err
	}
	input, copies, err := d.handOver(ctx, parent.SessionID, child, inv.GetCallId(), args)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			// 子 Session 在复制期间被删除（先写后查撤回了副本），或来源工件刚被删除。
			return d.result(ctx, inv.GetSessionId(), inv.GetRunId(), inv.GetCallId(), "cannot delegate: "+err.Error(), true)
		}
		return err
	}
	_, err = svc.SubmitDelegated(ctx, child, InputID(inv.GetCallId()), input, copies)
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

// withdrawChild 撤回对已有删除记录的子 Session 的创建。清理已完成时，创建只写了索引与 SessionCreated，原样删除即可；
// 清理尚未完成时再交给 Janitor 清除一遍（已有删除记录的 Session 每次都会被完整清理）：它若已删过日志，重建的日志
// 由这一遍兜住。
func (d *Delegator) withdrawChild(ctx context.Context, endUser, child string) error {
	svc := d.Service
	t, err := svc.Deletions.Get(ctx, child)
	if err != nil {
		return err
	}
	if !t.CompletedAt.IsZero() {
		if err := svc.Store.Log.Delete(ctx, child); err != nil {
			return err
		}
		if svc.Store.Snapshots != nil {
			if err := svc.Store.Snapshots.Delete(ctx, child); err != nil {
				return err
			}
		}
		if svc.Index != nil {
			if err := svc.Index.Delete(ctx, endUser, child); err != nil {
				return err
			}
		}
	}
	err = svc.Delete(ctx, child, t.Reason, t.RequestID)
	if errors.Is(err, service.ErrNotFound) {
		return nil // 日志与索引都已撤回：没有剩下的数据
	}
	return err
}

// checkArtifacts 检查委托点名的工件都属于父 Session；不属于时返回给模型的错误文字。
func (d *Delegator) checkArtifacts(ctx context.Context, parentSession string, refs []string) (string, error) {
	if len(refs) == 0 {
		return "", nil
	}
	if d.Artifacts == nil {
		return "cannot delegate: artifacts are not available", nil
	}
	for _, id := range refs {
		if _, err := d.Artifacts.Stat(ctx, parentSession, id); errors.Is(err, artifact.ErrNotFound) {
			// 别的 Session 的工件与不存在的同样回答，不透露它是否存在。
			return fmt.Sprintf("cannot delegate: artifact %s not found in this session", id), nil
		} else if err != nil {
			return "", err
		}
	}
	return "", nil
}

// handOver 组装子 Run 的输入：任务文字，加上复制进子 Session 的点名工件（父 → 子，ADR-0055）。任务已提交时
// 不再复制：提交按输入 ID 去重，首次的输入已经确定。
func (d *Delegator) handOver(ctx context.Context, parentSession, child, callID string, args Args) ([]*v1.ContentBlock, []*v1.ArtifactCopied, error) {
	input := model.TextBlocks(args.Input())
	if len(args.Artifacts) == 0 {
		return input, nil, nil
	}
	st, err := d.Service.Store.Load(ctx, child)
	if err != nil {
		return nil, nil, err
	}
	if st.Delegated != "" {
		return input, nil, nil
	}
	var copies []*v1.ArtifactCopied
	for _, src := range args.Artifacts {
		m, c, err := d.copy(ctx, parentSession, src, child, callID)
		if err != nil {
			return nil, nil, err
		}
		input = append(input, m.Block())
		copies = append(copies, c)
	}
	return input, copies, nil
}

// copy 把 srcSession 的工件复制进 dstSession，ID 由委托调用推导（CopyID），重复调用得到同一份副本。
func (d *Delegator) copy(ctx context.Context, srcSession, srcID, dstSession, callID string) (*artifact.Meta, *v1.ArtifactCopied, error) {
	dst, err := CopyID(dstSession, callID, srcID)
	if err != nil {
		return nil, nil, err
	}
	m, err := d.Artifacts.Copy(ctx, srcSession, srcID, dstSession, dst)
	if err != nil {
		return nil, nil, fmt.Errorf("artifact %s: %w", srcID, err)
	}
	return m, &v1.ArtifactCopied{CallId: callID, SourceSessionId: srcSession, SourceArtifactId: srcID, ArtifactId: m.ID}, nil
}

// Cancel 中断子 Session 中由这次委托开始的 Run（父 Run 被中断或委托超时）。子 Run 的中断照常级联到它自己的委托。
func (d *Delegator) Cancel(ctx context.Context, nodeID, callID string) error {
	child := ChildSession(nodeID)
	st, err := d.Service.Store.Load(ctx, child)
	if err != nil || st.Created == nil || st.Delegated == "" {
		return err
	}
	err = d.Service.InterruptBy(ctx, child, st.Delegated, "parent")
	if errors.Is(err, service.ErrNotFound) {
		return nil
	}
	return err
}

// Reconcile 在子 Session 的委托 Run 进行中、父调用却不再等待（父调用已有结果、父 Run 已终态或父 Session 已删除）时
// 中断委托 Run，返回 true。撤回委托（Cancel）是尽力而为的：撤回失败时子 Run 本会一直跑下去、白白消耗配额，
// 由子 Worker 在每次调用模型之前自己发现（评审 §11 问题 10）。读父 Session 出错时不中断：这只是清理，不能因为
// 存储抖动误停仍有人等待的任务。
func (d *Delegator) Reconcile(ctx context.Context, child *session.State) (bool, error) {
	p := child.Created.GetParent()
	r := child.Active()
	if p == nil || r == nil || r.ID != child.Delegated {
		return false, nil
	}
	if waiting, err := d.parentWaiting(ctx, p); err != nil || waiting {
		if err != nil {
			d.log().Warn("delegation reconcile skipped", "session", child.SessionID, "parent", p.GetSessionId(), "err", err)
		}
		return false, nil
	}
	err := d.Service.InterruptBy(ctx, child.SessionID, r.ID, ParentDone)
	if errors.Is(err, service.ErrNotFound) {
		err = nil
	}
	return true, err
}

// ParentDone 是子 Worker 发现父调用不再等待、自行中断委托 Run 时的中断方（RunInterrupted.by）。
const ParentDone = "parent_done"

// parentWaiting 报告父调用是否仍在等待结果。
func (d *Delegator) parentWaiting(ctx context.Context, p *v1.ParentRef) (bool, error) {
	svc := d.Service
	if gone, err := lifecycle.Deleted(ctx, svc.Deletions, p.GetSessionId()); err != nil || gone {
		return false, err
	}
	st, err := svc.Store.Load(ctx, p.GetSessionId())
	if err != nil {
		return false, err
	}
	if st.Background[p.GetCallId()] != nil && st.Closed == nil {
		return true, nil // 已转入后台、尚未了结（ADR-0058）：结果将作为一条输入送达
	}
	r := st.Run(p.GetRunId())
	if r == nil || r.Status.Terminal() {
		return false, nil // 已移出投影的 Run 都是终态
	}
	for _, c := range r.Calls {
		if c.Call.GetCallId() == p.GetCallId() {
			return !c.Done, nil
		}
	}
	return true, nil // 父 Run 尚未记下这次调用（不会发生：调用先于派发写入）；保守地视为在等
}

// Report 把子 Session 中委托 Run 的终态写回父调用（外部结果，与父 Session 的唤醒同一事务）。子 Worker 在子 Session
// 没有活跃 Run 时调用；重复调用无害：父调用已有结果、父 Run 已结束或父 Session 已删除时结果作废。
func (d *Delegator) Report(ctx context.Context, child *session.State) error {
	p := child.Created.GetParent()
	if p == nil {
		return nil
	}
	if child.Delegated == "" {
		return nil // 任务尚未提交（派发时会提交）
	}
	sum, ok := child.RunSummary(child.Delegated)
	if !ok || !sum.Status.Terminal() {
		return nil
	}
	text, media, err := d.lastMessage(ctx, child, child.Delegated)
	if err != nil {
		return err
	}
	switch sum.Status {
	case session.RunCompleted:
		content, copies, err := d.handBack(ctx, child.SessionID, p, text, media)
		if err != nil || content == nil {
			return err
		}
		return d.write(ctx, p.GetSessionId(), p.GetRunId(), p.GetCallId(), content, false, copies)
	case session.RunFailed:
		return d.result(ctx, p.GetSessionId(), p.GetRunId(), p.GetCallId(), "the delegated agent failed: "+text, true)
	default:
		return d.result(ctx, p.GetSessionId(), p.GetRunId(), p.GetCallId(), "the delegated task was interrupted", true)
	}
}

// lastMessage 是子 Run 最后一条模型回复的文字与媒体块（Run 可能已移出投影，读日志）。
func (d *Delegator) lastMessage(ctx context.Context, child *session.State, runID string) (string, []*v1.Media, error) {
	text := ""
	var media []*v1.Media
	err := session.ScanLog(ctx, d.Service.Store.Log, child.SessionID, func(e *v1.Event) bool {
		if m := e.GetAssistantMessage(); m.GetRunId() == runID {
			if t := model.Text(m.GetContent()); t != "" {
				text, media = t, nil
				for _, b := range m.GetContent() {
					if x := b.GetMedia(); x != nil {
						media = append(media, x)
					}
				}
			}
		}
		if f := e.GetRunFailed(); f.GetRunId() == runID {
			text, media = f.GetReason(), nil
		}
		return e.GetSeq() < child.Seq
	})
	if text == "" {
		text = "(no reply)"
	}
	return text, media, err
}

// handBack 组装写回父调用的结果（子 → 父，ADR-0055）：子 Run 最后一条回复的媒体附件与文字中引用的、属于子 Session
// 的工件复制进父 Session，文字中的工件 ID 换成副本的 ID，副本作为媒体块附在文字之后。至多复制 MaxArtifacts 个，
// 其余的引用原样留下（父 Run 打不开它们），文字末尾说明。父调用已不再等待时返回 nil 内容：不复制，结果本会作废。
func (d *Delegator) handBack(ctx context.Context, child string, p *v1.ParentRef, text string, media []*v1.Media) ([]*v1.ContentBlock, []*v1.ArtifactCopied, error) {
	var refs []string
	for _, m := range media {
		if id, ok := artifact.ParseURI(m.GetUri()); ok && !slices.Contains(refs, id) {
			refs = append(refs, id)
		}
	}
	for _, id := range artifactRef.FindAllString(text, -1) {
		if !slices.Contains(refs, id) {
			refs = append(refs, id)
		}
	}
	if len(refs) == 0 || d.Artifacts == nil {
		return model.TextBlocks(text), nil, nil
	}
	if waiting, err := d.parentWaiting(ctx, p); err != nil || !waiting {
		return nil, nil, err
	}
	var blocks []*v1.ContentBlock
	var copies []*v1.ArtifactCopied
	skipped := 0
	for _, src := range refs {
		// 只交回子 Session 自己的工件：模型写错的、别的 Session 的 ID 原样留在文字里。
		if _, err := d.Artifacts.Stat(ctx, child, src); errors.Is(err, artifact.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, nil, err
		}
		if len(copies) == MaxArtifacts {
			skipped++
			continue
		}
		m, c, err := d.copy(ctx, child, src, p.GetSessionId(), p.GetCallId())
		if errors.Is(err, artifact.ErrNotFound) {
			return nil, nil, nil // 父 Session 已删除（先写后查撤回了副本）：没有人等这个结果
		}
		if err != nil {
			return nil, nil, err
		}
		text = strings.ReplaceAll(text, src, m.ID)
		blocks = append(blocks, m.Block())
		copies = append(copies, c)
	}
	if skipped > 0 {
		text += fmt.Sprintf("\n\n[%d more artifacts were not handed over: at most %d per delegation]", skipped, MaxArtifacts)
	}
	return append(model.TextBlocks(text), blocks...), copies, nil
}

// result 写入父调用的文字结果，见 write。
func (d *Delegator) result(ctx context.Context, sessionID, runID, callID, text string, isErr bool) error {
	return d.write(ctx, sessionID, runID, callID, model.TextBlocks(text), isErr, nil)
}

// write 写入父调用的外部结果（连同复制进父 Session 的工件记录）并唤醒父 Session；父调用已转入后台时改为一条后台结果
// 输入（ADR-0058，service.DelegationResult）。调用已有结果或已了结、Run 已终态或 Session 已删除时静默作废（作废时
// 已复制的工件留在父 Session 中，随它删除）。
func (d *Delegator) write(ctx context.Context, sessionID, runID, callID string, content []*v1.ContentBlock, isErr bool, copies []*v1.ArtifactCopied) error {
	err := d.Service.DelegationResult(ctx, sessionID, runID, callID, content, isErr, copies)
	switch {
	case errors.Is(err, session.ErrInvalid):
		d.log().Info("delegation result dropped", "session", sessionID, "call", callID, "reason", err)
		return nil
	case errors.Is(err, session.ErrGone):
		return nil
	}
	return err
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
