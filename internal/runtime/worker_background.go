package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/node"
	"yanshi/internal/session"
)

// backgroundable 报告调用能否转入后台（ADR-0035）：只有路由到设备的调用。沙箱、用户本人与委托不是设备（Node ID 是平台
// 保留的）；委托派生的子 Session 不转后台：子 Run 结束后，迟到的结果回不到父 Run，由委托时限兜底。
func (w *Worker) backgroundable(c *session.Call) bool {
	return w.cfg.Devices != nil && w.cfg.BackgroundGrace > 0 && !node.Reserved(c.NodeID) && w.st.Created.GetParent() == nil
}

// background 把离线设备上的调用转入后台（ADR-0035）：先以新的截止时间重新派发（Inbox 按 call_id 只延长截止时间），再写入
// 占位结果，Run 照常继续。先派发：派发之后、写入之前崩溃时，调用仍在 Run 中等待，Inbox 中的项本来就在；反过来，写入之后、
// 派发之前若有人取消或了结了它，再派发就会把撤回的投递放回去。
func (w *Worker) background(ctx context.Context, r *session.Run, c *session.Call, info *node.Info) error {
	deadline := w.now().Add(w.cfg.BackgroundTimeout)
	res := w.toolResult(r, c.Call.GetCallId(), model.TextBlocks(BackgroundPlaceholder(info.Label)), false)
	res.GetToolResult().Backgrounded = &v1.Backgrounded{NodeId: c.NodeID, Deadline: timestamppb.New(deadline)}
	if ok, err := w.redispatch(ctx, r, c, deadline, res, time.Time{}); err != nil || !ok {
		return err
	}
	metrics.BackgroundCalls.WithLabelValues("backgrounded").Inc()
	w.cfg.Logger.Info("call backgrounded", "session", w.st.SessionID, "run", r.ID, "call", c.Call.GetCallId(), "node", c.NodeID)
	return nil
}

// BackgroundPlaceholder 是调用转入后台时交给模型的结果（ADR-0035）：调用没有失败，只是推迟；结果稍后作为单独的消息送达。
// 明确不要重试：重试的调用同样会排队，设备上线后执行两次。
func BackgroundPlaceholder(device string) string {
	return fmt.Sprintf("queued: device %q is offline, so this call has been queued and will run when the device comes back online. "+
		"Its result will arrive later as a separate message. Do not retry this call; tell the user it is queued and continue.", device)
}

// dueBackground 返回最早一个已到截止时间的未了结后台调用，没有时返回 nil。
func (w *Worker) dueBackground() *session.BackgroundCall {
	if w.cfg.Background == nil {
		return nil
	}
	if open := w.st.OpenBackground(); len(open) > 0 && !w.now().Before(open[0].Deadline) {
		return open[0]
	}
	return nil
}

// expireBackground 了结到期的后台调用：先撤回投递，之后设备的结果不再写入（网关只接受 Inbox 中的调用的结果），
// 再写入到期的后台结果输入。写入失败时下一步再撤回一次、再写一次，二者都幂等。
func (w *Worker) expireBackground(ctx context.Context, b *session.BackgroundCall) error {
	if err := w.cfg.Dispatch.Cancel(ctx, w.st.Created.GetEndUser(), b.NodeID, b.CallID); err != nil {
		return err
	}
	err := w.cfg.Background.ExpireBackground(ctx, w.st, b.CallID)
	if errors.Is(err, session.ErrGone) {
		w.drop()
		return nil
	}
	return err
}
