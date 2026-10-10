// Package notify 在需要 EndUser 注意、而他没有在看时，推送一条提醒（docs/design/m3-duplex-channel.md §9）：
// 待审批、待回答的提问、长 Run 结束。
//
// 规则：
//   - 有设备正把该 Session 显示在前台（在场记录 focused）时不推送：界面上已经能看到。
//   - 只推给一台设备：该 EndUser 最近在前台使用过、登记了推送的设备；推送失败时依次退到下一台。
//   - 推送内容只含种类与 ID，不含用户输入、模型输出或问题文字：推送经过第三方厂商通道，App 收到后再经连接取详情。
//
// 推送是尽力而为的：写入事件之后、推送之前进程崩溃，这条提醒就丢了；用户打开 App 时仍能从 Session 看到待办。
package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/presence"
)

// Kind 是提醒的种类。
type Kind string

const (
	Approval    Kind = "approval"
	Question    Kind = "question"
	RunFinished Kind = "run_finished"
)

// Notification 是一次需要 EndUser 注意的事情。
type Notification struct {
	Kind         Kind
	BusinessLine string
	EndUser      string
	SessionID    string
	RunID        string
	// CallID 是待审批或待回答的调用；RunFinished 时为空。
	CallID string
	// Failed 在 RunFinished 时表示 Run 以失败结束。
	Failed bool
}

// Device 是登记了推送的设备。
type Device struct {
	BusinessLine string
	EndUser      string
	// DeviceID 是连接的 Hello.node_id。
	DeviceID string
	Label    string
	Kind     string
	// Platform 是推送通道，如 "apns"、"fcm" 或厂商名；Token 是该通道的设备令牌（敏感，不写日志）。
	Platform string
	Token    string
	// LastActive 是该设备最近一次在前台使用（聚焦 Session、提交输入、审批、回答）的时间。
	LastActive time.Time
}

// Registry 保存 EndUser 的推送设备。它是 EndUser 数据（不属于任何 Session），注销账号时删除。
type Registry interface {
	// Register 登记或更新设备的推送令牌与标签，不改变 LastActive（新登记的设备取 at）。
	Register(ctx context.Context, d *Device, at time.Time) error
	// Touch 把设备的 LastActive 推进到 at（只前进不后退）；未登记的设备忽略。
	Touch(ctx context.Context, businessLine, endUser, deviceID string, at time.Time) error
	// List 返回 EndUser 的推送设备，按 LastActive 从近到远（相同时按 DeviceID）。
	List(ctx context.Context, businessLine, endUser string) ([]*Device, error)
	// Unregister 删除一台设备（令牌失效、用户退出登录）。
	Unregister(ctx context.Context, businessLine, endUser, deviceID string) error
	// DeleteEndUser 删除 EndUser 的全部推送设备（注销账号，ADR-0015）。
	DeleteEndUser(ctx context.Context, businessLine, endUser string) error
}

// ErrInvalidToken 表示通道拒绝了令牌（应用已卸载、令牌过期）：该设备随之撤销登记。
var ErrInvalidToken = errors.New("notify: invalid push token")

// Message 是发给设备的推送，只含种类与 ID。
type Message struct {
	Kind      Kind   `json:"kind"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
}

// Pusher 经厂商通道把消息推到一台设备。
type Pusher interface {
	Push(ctx context.Context, d *Device, m Message) error
}

// LogPusher 只记录推送（开发与测试）；真实通道见上线检查清单。
type LogPusher struct{ Logger *slog.Logger }

func (p LogPusher) Push(_ context.Context, d *Device, m Message) error {
	if p.Logger != nil {
		p.Logger.Info("push (stub)", "device", d.DeviceID, "platform", d.Platform, "kind", m.Kind, "session", m.SessionID, "call", m.CallID)
	}
	return nil
}

// Outcome 是一次提醒的处理结果。
type Outcome string

const (
	Pushed     Outcome = "pushed"
	Watching   Outcome = "watching"  // 有设备正显示该 Session，不推送
	NoDevice   Outcome = "no_device" // 没有可用的推送设备
	PushFailed Outcome = "failed"    // 所有设备都推送失败
)

type Notifier struct {
	Registry Registry
	Presence presence.Store
	Pusher   Pusher
	Clock    clock.Clock
	Logger   *slog.Logger
	// Timeout 是一次提醒（含逐台重试）的上限，默认 10 秒。
	Timeout time.Duration
}

func (n *Notifier) log() *slog.Logger {
	if n.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return n.Logger
}

// Notify 在后台处理一次提醒，不阻塞调用方（Worker 的执行路径）。
func (n *Notifier) Notify(ctx context.Context, x Notification) {
	timeout := n.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	go func() {
		defer cancel()
		if _, err := n.Deliver(ctx, x); err != nil {
			n.log().Warn("notification failed", "session", x.SessionID, "kind", x.Kind, "err", err)
		}
	}()
}

// Deliver 同步处理一次提醒，返回结果（测试与模拟直接调用它）。
func (n *Notifier) Deliver(ctx context.Context, x Notification) (Outcome, error) {
	if n.Presence != nil {
		entries, _, err := n.Presence.List(ctx, x.SessionID, n.Clock.Now())
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.Focused {
				return Watching, nil
			}
		}
	}
	devices, err := n.Registry.List(ctx, x.BusinessLine, x.EndUser)
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return NoDevice, nil
	}
	m := Message{Kind: x.Kind, SessionID: x.SessionID, RunID: x.RunID, CallID: x.CallID, Failed: x.Failed}
	for _, d := range devices {
		err := n.Pusher.Push(ctx, d, m)
		if err == nil {
			n.log().Info("notification pushed", "session", x.SessionID, "kind", x.Kind, "device", d.DeviceID)
			return Pushed, nil
		}
		if errors.Is(err, ErrInvalidToken) {
			if err := n.Registry.Unregister(ctx, d.BusinessLine, d.EndUser, d.DeviceID); err != nil {
				return "", err
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n.log().Warn("push failed, trying next device", "session", x.SessionID, "device", d.DeviceID, "err", err)
	}
	return PushFailed, nil
}
