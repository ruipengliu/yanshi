package domain

import (
	"fmt"
	"math"
	"slices"
	"time"
)

const DefaultCaptureTimeZone = "Asia/Shanghai"

// CaptureExpiresAt 使用当地日历运算，跨夏令时不能按 8*24 小时相加。
func CaptureExpiresAt(date, zone string) (time.Time, error) {
	location, ok := captureZone(zone)
	if !ok {
		return time.Time{}, fmt.Errorf("invalid business day time_zone")
	}
	day, err := time.ParseInLocation(time.DateOnly, date, location)
	if err != nil {
		return time.Time{}, fmt.Errorf("date must be YYYY-MM-DD")
	}
	return day.AddDate(0, 0, 1).AddDate(0, 0, 7).UTC(), nil
}

// SetCaptureClock 由入口绑定可信时钟，不序列化，也不随 Clone 带出事务。
// 不注入时使用真实时间，避免漏接的读取入口默许过期原文。
func (u *Book) SetCaptureClock(now func() time.Time) { u.captureClock = now }
func (u *Book) captureNow() time.Time {
	if u.captureClock != nil {
		return u.captureClock()
	}
	return time.Now()
}

// CheckCaptures 不依赖后台是否已清理；长期账本查询不调用这个闸门。
func (u *Book) CheckCaptures(date string) error {
	if day := u.captureDays[date]; day != nil {
		if day.Deleted {
			return ErrDayDeleted
		}
		if day.Expired || !day.ExpiresAt.IsZero() && !u.captureNow().Before(day.ExpiresAt) {
			return ErrCapturesExpired
		}
	}
	return nil
}

// ExpiringCaptureDays 只交回日期，发现和执行分开，执行时必须重查期限。
func (u *Book) ExpiringCaptureDays(now time.Time) []string {
	var dates []string
	for date, day := range u.captureDays {
		if !day.Expired && !day.Deleted && !day.ExpiresAt.IsZero() && !now.Before(day.ExpiresAt) {
			dates = append(dates, date)
		}
	}
	slices.Sort(dates)
	return dates
}

// ExpireCaptures 不经过 EraseDay：到期只清原文及读页状态，保留账本与处理结论。
// 缺口暂存正文在同一个事务清除，不能另留一份超过期限的原文。
func (u *Book) ExpireCaptures(date string, now time.Time) error {
	day := u.captureDays[date]
	if day == nil {
		return fmt.Errorf("business day does not exist")
	}
	if day.Expired || day.Deleted {
		return nil
	}
	if day.ExpiresAt.IsZero() || now.Before(day.ExpiresAt) {
		return fmt.Errorf("business day has not expired")
	}
	if err := day.checkCaptureRevision(); err != nil {
		return err
	}
	if u.captures == nil {
		return ErrCaptureTransactionClosed
	}
	if err := u.captures.DeleteDay(date); err != nil {
		return err
	}
	if err := u.captures.ClearPending(date); err != nil {
		return err
	}
	u.invalidateCaptureStreams(date)
	day.Expired = true
	day.CaptureRevision++
	day.ReadThrough, day.GapMS, day.GapSet = Ref{}, 0, false
	return nil
}

// 版本不可绕回旧值，否则在途简述可能把不同原文误认为同一版。
func (d *captureDay) checkCaptureRevision() error {
	if d.CaptureRevision == math.MaxUint64 {
		return fmt.Errorf("capture revision exhausted")
	}
	return nil
}
