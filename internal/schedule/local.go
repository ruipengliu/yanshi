package schedule

import (
	"fmt"
	"time"
)

// FormatLocal 以同一 loc 中的 ref 年份判断是否显示年份；不读取系统时间。
// loc 为 nil 时使用 UTC，避免隐式依赖进程的 Local 设置。
func FormatLocal(t time.Time, loc *time.Location, ref time.Time) string {
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	prefix := ""
	if t.Year() != ref.In(loc).Year() {
		prefix = fmt.Sprintf("%d年", t.Year())
	}
	weekdays := [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}
	return fmt.Sprintf("%s%d月%d日%s %02d:%02d", prefix, t.Month(), t.Day(), weekdays[t.Weekday()], t.Hour(), t.Minute())
}

// civilDate 用 UTC 装载当地日历字段，仅用于日历运算，不代表真实到期时刻。
func civilDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

type zone struct {
	begin  time.Time
	end    time.Time
	offset int
}

// zones 读取真实的偏移区间，而不是让 time.Date 猜测缺口或重复时间。
// IANA 的偏移与整日跳变都在两天余量内；一个当地日只需读取一次。
func zones(wall time.Time, loc *time.Location) []zone {
	begin, end := civilDate(wall).Add(-48*time.Hour), civilDate(wall).Add(72*time.Hour)
	var result []zone
	for cursor := begin; cursor.Before(end); {
		local := cursor.In(loc)
		_, offset := local.Zone()
		_, bound := local.ZoneBounds()
		if bound.IsZero() || bound.After(end) {
			bound = end
		}
		result = append(result, zone{cursor, bound, offset})
		cursor = bound
	}
	return result
}

func resolve(wall time.Time, spans []zone) time.Time {
	var first, following, followingWall time.Time
	for _, span := range spans {
		candidate := wall.Add(-time.Duration(span.offset) * time.Second)
		if !candidate.Before(span.begin) && candidate.Before(span.end) {
			if first.IsZero() || candidate.Before(first) {
				first = candidate
			}
		}
		localBegin := span.begin.Add(time.Duration(span.offset) * time.Second)
		if localBegin.After(wall) && (followingWall.IsZero() || localBegin.Before(followingWall)) {
			followingWall, following = localBegin, span.begin
		}
	}
	if !first.IsZero() {
		return first.UTC()
	}
	// 缺口内的所有钟点都指向缺口结束，不能保留原来的分钟偏移。
	return following.UTC()
}
