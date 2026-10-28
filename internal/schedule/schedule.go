// Package schedule 校验定时声明、计算到期与错过的合并，并显示当地时间。
// 时间由调用方传入，使 Worker 的单步与重放不依赖系统时钟；本包不保存进度。
// Compile 保证周期声明在创建后的十年内、截止与次数限制内至少有一次到期。
package schedule

import (
	"sort"
	"time"
)

// Skipped 是被最近一次到期替代的一段；Count 为零时两个端点都是零值。
type Skipped struct {
	First time.Time
	Last  time.Time
	Count int64
}

// Catchup 中的时间均为 UTC；零值 Fire 或 Next 分别表示没有到期或没有后续。
type Catchup struct {
	Fire    time.Time
	Skipped Skipped
	Next    time.Time
}

// Next 返回严格晚于 after 的首次到期。COUNT 从 start 之后的第一次开始计数。
// 搜索截止于 max(start, after) 后十年；Compile 已校验创建时的首次到期存在。
func (p *Plan) Next(after time.Time) (time.Time, bool) {
	after = after.UTC()
	if p.OneShot() {
		if p.at.After(after) {
			return p.at, true
		}
		return time.Time{}, false
	}
	if after.Before(p.start) {
		after = p.start
	}
	end := after.AddDate(10, 0, 0)
	if !p.until.IsZero() && p.until.Before(end) {
		end = p.until
	}
	if !end.After(after) {
		return time.Time{}, false
	}
	from := after
	if p.count != 0 {
		from = p.start
	}
	var result time.Time
	p.walk(from, end, func(batch dayBatch, lo, hi int) bool {
		index := batch.after(after)
		if index < lo {
			index = lo
		}
		if index < hi {
			result = batch.at(index)
			return false
		}
		return true
	})
	return result, !result.IsZero()
}

// Catchup 只执行 (last, now] 中最后一次，其余合并为跳过。
// 从未处理过时 last 传 Start；now < last 时不回退进度，不返回 Fire。
// 普通日期按钟点数组的上下界直接计数，只有偏移变化的日期才枚举（每天至多 24 个）。
func (p *Plan) Catchup(last, now time.Time) Catchup {
	var result Catchup
	if next, ok := p.Next(now); ok {
		result.Next = next
	}
	if last.Before(p.start) {
		last = p.start
	}
	if now.Before(last) {
		return result
	}
	if p.OneShot() {
		if p.at.After(last) && !p.at.After(now) {
			result.Fire = p.at
		}
		return result
	}
	from := last
	if p.count != 0 {
		from = p.start
	}
	p.walk(from, now, func(batch dayBatch, lo, hi int) bool {
		index := batch.after(last)
		if index > lo {
			lo = index
		}
		if lo >= hi {
			return true
		}
		if !result.Fire.IsZero() {
			if result.Skipped.Count == 0 {
				result.Skipped.First = result.Fire
			}
			result.Skipped.Last = result.Fire
			result.Skipped.Count++
		}
		if hi-lo > 1 {
			if result.Skipped.Count == 0 {
				result.Skipped.First = batch.at(lo)
			}
			result.Skipped.Last = batch.at(hi - 2)
			result.Skipped.Count += int64(hi - lo - 1)
		}
		result.Fire = batch.at(hi - 1)
		return true
	})
	return result
}

// walk 按日批量访问，不对每次到期重新搜索。COUNT 限制包括已处理的到期。
// 日期前后留两天用于跨日期的 IANA 跳变，prev 消除缺口顺延产生的重复 UTC。
func (p *Plan) walk(from, end time.Time, visit func(dayBatch, int, int) bool) {
	if from.Before(p.start) {
		from = p.start
	}
	if !p.until.IsZero() && p.until.Before(end) {
		end = p.until
	}
	if end.Before(from) {
		return
	}
	day := civilDate(from.In(p.loc)).AddDate(0, 0, -2)
	lastDay := civilDate(end.In(p.loc)).AddDate(0, 0, 2)
	prev := from
	used := 0
	for ; !day.After(lastDay); day = day.AddDate(0, 0, 1) {
		if !p.matches(day) {
			continue
		}
		batch := p.batch(day)
		lo, hi := batch.after(prev), batch.after(end)
		if p.count != 0 && hi-lo > p.count-used {
			hi = lo + p.count - used
		}
		if hi <= lo {
			continue
		}
		used += hi - lo
		prev = batch.at(hi - 1)
		if !visit(batch, lo, hi) || (p.count != 0 && used >= p.count) {
			return
		}
	}
}

type dayBatch struct {
	base    time.Time
	minutes []int
	times   []time.Time
}

func (b dayBatch) len() int {
	if b.times != nil {
		return len(b.times)
	}
	return len(b.minutes)
}

func (b dayBatch) at(i int) time.Time {
	if b.times != nil {
		return b.times[i]
	}
	return b.base.Add(time.Duration(b.minutes[i]) * time.Minute)
}

func (b dayBatch) after(t time.Time) int {
	return sort.Search(b.len(), func(i int) bool { return b.at(i).After(t) })
}

func (p *Plan) batch(day time.Time) dayBatch {
	spans := zones(day, p.loc)
	var regular *zone
	for i := range spans {
		span := &spans[i]
		offset := time.Duration(span.offset) * time.Second
		localBegin, localEnd := span.begin.Add(offset), span.end.Add(offset)
		if !localBegin.Before(day.Add(24*time.Hour)) || !localEnd.After(day) {
			continue
		}
		// 午夜回拨时，较晚的偏移区间可能覆盖整天，但前一偏移也覆盖零点。
		// 必须只有一个相交区间，才能省略逐个钟点的歧义消解。
		if regular != nil {
			regular = nil
			break
		}
		regular = span
	}
	if regular != nil {
		base := day.Add(-time.Duration(regular.offset) * time.Second)
		if !base.Before(regular.begin) && !base.Add(24*time.Hour).After(regular.end) {
			return dayBatch{base: base, minutes: p.minutes}
		}
	}
	values := make([]time.Time, 0, len(p.minutes))
	for _, minute := range p.minutes {
		values = append(values, resolve(day.Add(time.Duration(minute)*time.Minute), spans))
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Before(values[j]) })
	unique := values[:0]
	for _, value := range values {
		if len(unique) == 0 || !value.Equal(unique[len(unique)-1]) {
			unique = append(unique, value)
		}
	}
	return dayBatch{times: unique}
}

func (p *Plan) matches(day time.Time) bool {
	if day.Before(p.anchor) {
		return false
	}
	days := (day.Unix() - p.anchor.Unix()) / 86400
	switch p.freq {
	case "DAILY":
		if days%int64(p.interval) != 0 {
			return false
		}
	case "WEEKLY":
		mondayOffset := (int(p.anchor.Weekday()) + 6) % 7
		if ((days+int64(mondayOffset))/7)%int64(p.interval) != 0 {
			return false
		}
	case "MONTHLY":
		months := (day.Year()-p.anchor.Year())*12 + int(day.Month()) - int(p.anchor.Month())
		if months%p.interval != 0 {
			return false
		}
	}
	return p.matchesDate(day)
}

// BYDAY 与 BYMONTHDAY 同时出现时取交集；各自的列表内部取并集。
func (p *Plan) matchesDate(day time.Time) bool {
	monthEnd := time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if len(p.monthDays) != 0 {
		found := false
		for _, date := range p.monthDays {
			if date == day.Day() || (date == -1 && day.Day() == monthEnd) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if len(p.days) != 0 {
		for _, weekday := range p.days {
			if weekday.day != day.Weekday() {
				continue
			}
			if weekday.ordinal == 0 || weekday.ordinal == (day.Day()-1)/7+1 || (weekday.ordinal == -1 && day.Day()+7 > monthEnd) {
				return true
			}
		}
		return false
	}
	return true
}

// 公历每 400 年重复。检查整轮相位，可拒绝永远落在小月的 31 日等空序列，
// 而不会把十年搜索范围以外仍有日期的规则误报为无效。
func (p *Plan) possible() bool {
	switch p.freq {
	case "DAILY":
		for i := 0; i < 7; i++ {
			if p.matchesDate(p.anchor.AddDate(0, 0, i*p.interval)) {
				return true
			}
		}
	case "WEEKLY":
		return true
	case "MONTHLY":
		month := time.Date(p.anchor.Year(), p.anchor.Month(), 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 4800; i++ {
			for day := month; day.Month() == month.Month(); day = day.AddDate(0, 0, 1) {
				if p.matchesDate(day) {
					return true
				}
			}
			month = month.AddDate(0, p.interval, 0)
		}
	}
	return false
}
