// Package lifecycle 管理 Session 的关闭与删除（docs/design/m2-session-lifecycle.md，ADR-0015）。
//
// Index 是 Session 索引：日志的投影，可由日志重建，用于列表、按 EndUser 查找与保留策略。
// Deletions 是删除记录：写入即生效，之后该 Session 对外不可见，任何组件都不得再为它写入数据；
// 物理清理由 Janitor 在后台完成。
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("lifecycle: not found")

// TouchEvery 是 Session 索引中最后输入时间的精度：与上一个 Run 落在同一个 TouchEvery 时段内的输入不更新索引，
// 省掉连续对话中每个 Run 的一次写入（延展性评审 §4.3）。索引因此至多滞后 TouchEvery，按空闲时长关闭的保留策略
// 多等这么久，不会提前关闭。
const TouchEvery = time.Minute

// Retention 是业务线的保留策略（docs/design/m2-session-lifecycle.md §5）；零值表示不自动处理。
type Retention struct {
	// CloseAfterIdle 是最后一次输入后多久自动关闭。
	CloseAfterIdle time.Duration `yaml:"close_after_idle"`
	// DeleteAfterClose 是关闭后多久自动删除。
	DeleteAfterClose time.Duration `yaml:"delete_after_close"`
}

// Session 是 Session 索引中的一行。
type Session struct {
	ID           string
	BusinessLine string
	EndUser      string
	Agent        string
	CreatedAt    time.Time
	LastInputAt  time.Time
	// ClosedAt 为零值表示未关闭。
	ClosedAt time.Time
}

type Index interface {
	// Put 写入新 Session；已存在时不变（幂等）。
	Put(ctx context.Context, s *Session) error
	// Touch 把最后输入时间推进到 at（只增不减）。
	Touch(ctx context.Context, id string, at time.Time) error
	// MarkClosed 记录关闭时间；已关闭时不变。
	MarkClosed(ctx context.Context, id string, at time.Time) error
	Get(ctx context.Context, id string) (*Session, error)
	// List 返回业务线中 endUser（为空表示全部）的 Session，按最后输入时间倒序；
	// cursor 为上一页返回的 next，空串表示第一页；没有更多时 next 为空。
	List(ctx context.Context, businessLine, endUser, cursor string, limit int) (page []*Session, next string, err error)
	// IDsOf 返回 EndUser 在业务线下的全部 Session ID。
	IDsOf(ctx context.Context, businessLine, endUser string) ([]string, error)
	// IdleBefore 返回业务线中未关闭、最后输入早于 t 的 Session，至多 limit 个。
	IdleBefore(ctx context.Context, businessLine string, t time.Time, limit int) ([]string, error)
	// ClosedBefore 返回业务线中关闭时间早于 t 的 Session，至多 limit 个。
	ClosedBefore(ctx context.Context, businessLine string, t time.Time, limit int) ([]string, error)
	Delete(ctx context.Context, id string) error
}

// Tombstone 是一条删除记录。不含 EndUser：删除完成后不应再留存个人标识。
type Tombstone struct {
	SessionID    string
	BusinessLine string
	// Reason 如 "end_user"、"account_deletion"、"retention"。
	Reason string
	// RequestID 非空表示属于一次按 EndUser 的删除请求。
	RequestID   string
	RequestedAt time.Time
	// CompletedAt 为零值表示清理尚未完成。
	CompletedAt time.Time
}

// Request 是一次按 EndUser 的删除请求（注销账号）的进度。
type Request struct {
	ID           string
	BusinessLine string
	CreatedAt    time.Time
	Sessions     int
	Completed    int
}

type Deletions interface {
	// Mark 写入删除记录；已存在时不变并返回 false。
	Mark(ctx context.Context, t *Tombstone) (bool, error)
	Get(ctx context.Context, sessionID string) (*Tombstone, error)
	Complete(ctx context.Context, sessionID string, at time.Time) error
	// Pending 返回尚未完成清理的删除记录，至多 limit 个（Janitor 据此兜底重试）。
	Pending(ctx context.Context, limit int) ([]string, error)
	CreateRequest(ctx context.Context, r *Request) error
	// Request 返回删除请求及其进度。
	Request(ctx context.Context, id string) (*Request, error)
}

// Watcher 是 Deletions 的可选接口：WaitDeleted 阻塞到 Session 有删除记录为止（返回 nil），或 ctx 结束。
// 事件流据此在 Session 删除时主动结束，而不必每隔一段时间查询一次删除记录：订阅者多时，周期查询本身就是可观的负载
// （延展性评审 §4.3）。
type Watcher interface {
	WaitDeleted(ctx context.Context, sessionID string) error
}

// Deleted 报告 Session 是否已有删除记录（不论清理是否完成）。
func Deleted(ctx context.Context, d Deletions, sessionID string) (bool, error) {
	_, err := d.Get(ctx, sessionID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// EncodeCursor 与 DecodeCursor 编码列表位置：最后输入时间（纳秒）与 Session ID，二者共同确定唯一顺序。
func EncodeCursor(s *Session) string {
	return strconv.FormatInt(s.LastInputAt.UnixNano(), 10) + ":" + s.ID
}

func DecodeCursor(c string) (time.Time, string, error) {
	n, id, ok := strings.Cut(c, ":")
	ns, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil {
		return time.Time{}, "", fmt.Errorf("lifecycle: invalid cursor %q", c)
	}
	return time.Unix(0, ns).UTC(), id, nil
}

// before 报告 s 在倒序列表中是否排在游标 (t, id) 之后。
func before(s *Session, t time.Time, id string) bool {
	return s.LastInputAt.Before(t) || (s.LastInputAt.Equal(t) && s.ID < id)
}

// Guard 判断 Session 是否已结束（关闭或删除），供"先写、后查"的写入方使用：
// 写入后查到已结束，就撤回自己刚写下的数据（ADR-0015）。
type Guard struct {
	Index     Index
	Deletions Deletions
}

// Ended 报告 Session 是否已关闭或已删除。索引中没有该 Session 时只看删除记录。
func (g Guard) Ended(ctx context.Context, sessionID string) (bool, error) {
	if g.Deletions != nil {
		if gone, err := Deleted(ctx, g.Deletions, sessionID); err != nil || gone {
			return gone, err
		}
	}
	if g.Index == nil {
		return false, nil
	}
	s, err := g.Index.Get(ctx, sessionID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !s.ClosedAt.IsZero(), nil
}
