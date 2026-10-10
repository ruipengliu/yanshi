package sim

import (
	"context"
	"errors"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/eventlog"
	"yanshi/internal/notify"
)

// 提醒的模拟（docs/design/m3-duplex-channel.md §9）：Worker 的提醒同步交给真实的 notify.Notifier 决定，
// 推送通道随机失败或报告令牌无效。不变量：每条提醒对应日志中刚写入的事件；同一件事至多提醒一次
// （挂起后的重复检查、接管后的重放都不再提醒）。

type simNotifier struct{ w *World }

func (s simNotifier) Notify(ctx context.Context, n notify.Notification) {
	w := s.w
	key := fmt.Sprintf("%s/%s/%s/%s", n.SessionID, n.Kind, n.RunID, n.CallID)
	if w.notified[key] {
		w.violations = append(w.violations, "notified twice: "+key)
	}
	w.notified[key] = true
	if err := w.checkNotification(ctx, n); err != nil {
		w.violations = append(w.violations, err.Error())
	}
	out, err := w.notifier.Deliver(ctx, n)
	if err != nil {
		w.violations = append(w.violations, fmt.Sprintf("notification %s: %v", key, err))
		return
	}
	switch out {
	case notify.Pushed:
		w.Stats.Notifications++
	case notify.Watching:
		w.Stats.NotificationsWatching++
	}
	w.tracef("notify %s → %s", key, out)
}

// checkNotification 检查提醒对应日志中已写入的事件。
func (w *World) checkNotification(ctx context.Context, n notify.Notification) error {
	events, err := eventlog.ReadAll(ctx, w.log, n.SessionID, 0)
	if err != nil {
		return err
	}
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *v1.Event_ApprovalRequested:
			if n.Kind == notify.Approval && p.ApprovalRequested.GetCallId() == n.CallID {
				return nil
			}
		case *v1.Event_ToolCallStarted:
			if n.Kind == notify.Question && p.ToolCallStarted.GetCallId() == n.CallID && p.ToolCallStarted.GetNodeId() == askuser.NodeID {
				return nil
			}
		case *v1.Event_RunCompleted, *v1.Event_RunFailed:
			if n.Kind == notify.RunFinished && e.GetRunCompleted().GetRunId()+e.GetRunFailed().GetRunId() == n.RunID {
				return nil
			}
		}
	}
	return fmt.Errorf("invariant: notification %s/%s for %s has no committed event", n.Kind, n.CallID+n.RunID, n.SessionID)
}

// simPusher 是推送通道：故障模式下随机失败或报告令牌无效（设备随之撤销登记，重连时重新登记）。
type simPusher struct{ w *World }

func (p simPusher) Push(context.Context, *notify.Device, notify.Message) error {
	w := p.w
	if w.faults {
		switch x := w.rng.Float64(); {
		case x < 0.1:
			w.Stats.PushFailures++
			return errors.New("simulated vendor outage")
		case x < 0.15:
			w.Stats.PushFailures++
			return notify.ErrInvalidToken
		}
	}
	return nil
}
