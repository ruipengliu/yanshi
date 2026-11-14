package domain

import (
	"maps"
	"slices"
	"time"
)

// CaptureSchedule 只交出编排所需的日期、版本与进度，不把原文带出事务。
type CaptureSchedule struct {
	BusinessDay
	Date               string
	LastBatchAt        time.Time
	LastVisibleBatchAt time.Time
	Revision           uint64
	Processed          bool
}

func (u *Book) SchedulingDays() []CaptureSchedule {
	out := []CaptureSchedule{}
	for _, date := range slices.Sorted(maps.Keys(u.captureDays)) {
		d := u.captureDays[date]
		if d.ExpiresAt.IsZero() {
			continue
		}
		revision := d.CaptureRevision
		// 自然到期清理不代表新原文；失败任务身份在清理前后保持相同。
		if d.Expired && revision > 0 {
			revision--
		}
		out = append(out, CaptureSchedule{BusinessDay: u.BusinessDay(date), Date: date,
			LastBatchAt: d.LastBatchAt, Revision: revision,
			LastVisibleBatchAt: d.lastVisibleBatchAt(),
			Processed:          d.processed()})
	}
	return out
}

// AcceptedCommand 只判断接受事实，不能用 Run 的完成回执替代。
func (u *Book) AcceptedCommand(kind, date, inputID string) bool {
	f := u.acceptedFacts[factID(kind, date)]
	return f.Active && f.CommandID == inputID
}

// WeekProjects 用接受事实的固定窗口找项目，尚未创建 Session 的项目也要归集。
func (u *Book) WeekProjects(from string) []string {
	d, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return nil
	}
	until := d.AddDate(0, 0, 7).Format(time.DateOnly)
	seen := map[string]bool{}
	for _, e := range u.exchanges {
		if !e.Live() || e.Date < from || e.Date >= until {
			continue
		}
		for _, id := range e.InProjects() {
			if p := u.project(id); p != nil && p.Active() {
				seen[id] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// PrepareScheduling 为 A10 前的业务日补齐无正文元数据。历史接收时刻不可推断，
// 首次发现保守地从当前时刻等待静默；已经到期的原文绝不为补元数据而读取。
func (u *Book) PrepareScheduling() error {
	for date, d := range u.captureDays {
		if d.Deleted || d.Expired || d.ExpiresAt.IsZero() || !u.captureNow().Before(d.ExpiresAt) {
			continue
		}
		if d.LastBatchAt.IsZero() {
			access, err := u.captureAccess()
			if err != nil {
				return err
			}
			last, _, err := access.Adjacent(date, Ref{}, true)
			if err != nil {
				return err
			}
			d.Tail, d.LastBatchAt = last.Ref, u.captureNow().UTC()
		}
		if err := u.prepareVisibleCaptures(date); err != nil {
			return err
		}
	}
	return nil
}

// 调度与产品页面使用同一处理完成判定，不能把 Run 终态当成整理完成。
func (d *captureDay) processed() bool {
	progress, _ := parseCaptureCursor(d.Progress)
	if d.VisibleTail != nil {
		return d.VisibleTail.Seq == 0 || refLE(*d.VisibleTail, progress.Last)
	}
	return d.Tail.Seq == 0 && (d.Next == 0 || !d.LastBatchAt.IsZero()) || d.Tail.Seq != 0 && refLE(d.Tail, progress.Last)
}

func (d *captureDay) lastVisibleBatchAt() time.Time {
	if d.VisibleTail == nil {
		return d.LastBatchAt
	}
	return d.LastVisibleBatchAt
}
