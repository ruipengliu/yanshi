// Package presence 记录哪些设备正在看某个 Session（在场，docs/design/m3-duplex-channel.md §6）。
//
// 在场是易失的：不进 Event 日志，记录带有效期，由连接定期续期；进程崩溃留下的记录过期后自然消失。
// 记录含有 Session ID，属于 Session 数据：写入"先写、后查"删除记录，Janitor 按 Session 删除（ADR-0015）。
package presence

import (
	"context"
	"slices"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
)

const (
	// TTL 是记录的有效期；连接每 Heartbeat 续期一次，进程崩溃时最迟 TTL 后消失。
	TTL       = 90 * time.Second
	Heartbeat = 30 * time.Second
	// TypingFor 是一次"正在输入"的有效时长：客户端持续输入时应在此之前重发。
	TypingFor = 10 * time.Second
)

// Entry 是一条连接对一个 Session 的在场记录。
type Entry struct {
	SessionID string
	// ConnID 标识一条连接，由网关生成。
	ConnID string
	// DeviceID 是连接的 Hello.node_id；同一设备的多条连接合并显示。
	DeviceID string
	Label    string
	Kind     string
	// Focused 表示该 Session 正显示在这台设备的前台。
	Focused bool
	// TypingUntil 之前视为正在输入。
	TypingUntil time.Time
	// Expires 之后视为已离开。
	Expires time.Time
}

// visible 报告两条记录对外显示的内容是否相同（不比较有效期）。
func (e *Entry) visible(o *Entry) bool {
	return e.DeviceID == o.DeviceID && e.Label == o.Label && e.Kind == o.Kind && e.Focused == o.Focused &&
		e.TypingUntil.Equal(o.TypingUntil)
}

type Store interface {
	// Put 按 (SessionID, ConnID) 写入或覆盖一条记录。显示内容（有效期之外的字段）有变化时版本递增并通知等待者。
	Put(ctx context.Context, e *Entry) error
	// Touch 把连接 connID 的全部记录续期到 expires，不改变版本。
	Touch(ctx context.Context, connID string, expires time.Time) error
	// Remove 删除一条记录；记录存在时版本递增。
	Remove(ctx context.Context, sessionID, connID string) error
	// List 返回 Session 中 Expires 晚于 now 的记录（按 ConnID 排序）与当前版本。
	List(ctx context.Context, sessionID string, now time.Time) ([]*Entry, uint64, error)
	// Wait 阻塞直到版本不再等于 version，或 ctx 结束。
	Wait(ctx context.Context, sessionID string, version uint64) error
	// DeleteSession 删除 Session 的全部记录（ADR-0015）。
	DeleteSession(ctx context.Context, sessionID string) error
	// Prune 删除 Expires 不晚于 before 的记录。
	Prune(ctx context.Context, before time.Time) error
}

// Service 是在场记录的写入入口。
type Service struct {
	Store     Store
	Deletions lifecycle.Deletions
	Clock     clock.Clock
}

// Put 写入记录，然后复查删除记录：Session 已删除时撤回它（先写、后查，ADR-0015）。
func (s *Service) Put(ctx context.Context, e *Entry) error {
	if err := s.Store.Put(ctx, e); err != nil {
		return err
	}
	if s.Deletions == nil {
		return nil
	}
	gone, err := lifecycle.Deleted(ctx, s.Deletions, e.SessionID)
	if err != nil || !gone {
		return err
	}
	return s.Store.DeleteSession(ctx, e.SessionID)
}

// Viewers 把记录按设备合并为对外的在场列表（按标签、设备排序）：同一设备任一连接聚焦或正在输入即算。
func Viewers(entries []*Entry, now time.Time) []*v1.Viewer {
	byDevice := map[string]*v1.Viewer{}
	for _, e := range entries {
		if !e.Expires.After(now) {
			continue
		}
		v := byDevice[e.DeviceID]
		if v == nil {
			v = &v1.Viewer{DeviceId: e.DeviceID, Label: e.Label, Kind: e.Kind}
			byDevice[e.DeviceID] = v
		}
		v.Focused = v.Focused || e.Focused
		v.Typing = v.Typing || e.TypingUntil.After(now)
	}
	out := make([]*v1.Viewer, 0, len(byDevice))
	for _, v := range byDevice {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b *v1.Viewer) int {
		if c := strings.Compare(a.GetLabel(), b.GetLabel()); c != 0 {
			return c
		}
		return strings.Compare(a.GetDeviceId(), b.GetDeviceId())
	})
	return out
}

// NextChange 返回 now 之后最早一个会使 Viewers 结果变化的时刻（记录过期、输入状态过期）；没有时为零值。
func NextChange(entries []*Entry, now time.Time) time.Time {
	var next time.Time
	consider := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, e := range entries {
		consider(e.Expires)
		consider(e.TypingUntil)
	}
	return next
}
