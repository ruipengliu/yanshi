// Package schedule 决定固定业务日的整理时刻；不读取内核运行状态，也不产生随机数。
package schedule

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"time"
)

const (
	DaySilence     = 5 * time.Minute
	EveningSilence = 2 * time.Hour
	ScanInterval   = 30 * time.Second
	MaxAttempts    = 5
	RetryBase      = 5 * time.Minute
	RetryMax       = time.Hour
)

// Jitter 按完整 owner 与日期派生，避免分隔符碰撞；所有实例和重启得到同一秒。
func Jitter(bl, eu, date string) time.Duration {
	data, _ := json.Marshal([]string{bl, eu, date})
	h := sha256.Sum256(data)
	return time.Duration(binary.BigEndian.Uint64(h[:8])%1800) * time.Second
}

// DayTimes 用当地日历确定午夜，夏令时日不能用固定 24 小时跨日。
func DayTimes(date string, zone *time.Location, last time.Time, jitter time.Duration) (daytime, final time.Time, err error) {
	d, err := time.ParseInLocation(time.DateOnly, date, zone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if last.IsZero() {
		last = d
	}
	daytime = last.Add(DaySilence)
	evening := time.Date(d.Year(), d.Month(), d.Day(), 18, 0, 0, 0, zone)
	next := d.AddDate(0, 0, 1)
	latest := time.Date(next.Year(), next.Month(), next.Day(), 4, 0, 0, 0, zone).Add(jitter)
	final = last.Add(EveningSilence)
	if final.Before(evening) {
		final = evening
	}
	if final.After(latest) {
		final = latest
	}
	return daytime, final, nil
}

// Week 返回最近已到点的周日及其七天起点，错过当晚可在重启后补做。
func Week(now time.Time, zone *time.Location, bl, eu string) (from string, due time.Time) {
	n := now.In(zone)
	sunday := time.Date(n.Year(), n.Month(), n.Day(), 23, 0, 0, 0, zone).AddDate(0, 0, -int(n.Weekday()))
	for {
		from = sunday.AddDate(0, 0, -6).Format(time.DateOnly)
		due = sunday.Add(Jitter(bl, eu, from))
		if !due.After(now) {
			return from, due
		}
		sunday = sunday.AddDate(0, 0, -7)
	}
}
