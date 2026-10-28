package schedule

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid 表示声明无法执行；包装的中文原因可直接供创建方修改声明。
var ErrInvalid = errors.New("invalid schedule")

// Spec 是创建方的声明；TimeZone 的 Session 默认值由调用方补齐。
type Spec struct {
	At       string
	RRule    string
	TimeZone string
	Until    time.Time
}

// Plan 不保存执行进度。字段不导出，避免调用方改变锚点或已校验的规则。
type Plan struct {
	spec      Spec
	loc       *time.Location
	start     time.Time
	at        time.Time
	until     time.Time
	anchor    time.Time
	freq      string
	interval  int
	count     int
	days      []weekday
	monthDays []int
	minutes   []int
}

type weekday struct {
	day     time.Weekday
	ordinal int
}

// Spec 返回可用于持久化、重新 Compile 的声明副本。
func (p *Plan) Spec() Spec { return p.spec }

// Loc 返回声明使用的时区。
func (p *Plan) Loc() *time.Location { return p.loc }

// Start 返回创建时刻（UTC）；重建时必须使用同一个锚点。
func (p *Plan) Start() time.Time { return p.start }

// OneShot 区分一次性与周期定时。
func (p *Plan) OneShot() bool { return !p.at.IsZero() }

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

// Compile 校验声明；start 由调用方的 clock.Clock 提供，本包不读取系统时间。
func Compile(spec Spec, start time.Time) (*Plan, error) {
	if (spec.At == "") == (spec.RRule == "") {
		return nil, invalid("at 与 rrule 须且只能填写一个")
	}
	// Local 依赖进程环境，不能作为可重放的 IANA 时区。
	if spec.TimeZone == "" || spec.TimeZone == "Local" {
		return nil, invalid("time_zone 须填写 IANA 时区，例如 Asia/Shanghai")
	}
	loc, err := time.LoadLocation(spec.TimeZone)
	if err != nil {
		return nil, invalid("time_zone 无效，请填写 IANA 时区，例如 Asia/Shanghai")
	}
	p := &Plan{spec: spec, loc: loc, start: start.UTC(), interval: 1}
	p.anchor = civilDate(start.In(loc))
	if !spec.Until.IsZero() {
		p.until = spec.Until.UTC()
		p.spec.Until = p.until
		if !p.until.After(p.start) {
			return nil, invalid("until 须晚于现在")
		}
	}
	if spec.At != "" {
		wall, err := parseWall(spec.At, "2006-01-02T15:04", "2006-01-02T15:04:05")
		if err != nil {
			return nil, invalid("at 须为不带偏移的当地时间，例如 2026-10-30T17:00 或 2026-10-30T17:00:00")
		}
		p.at = resolve(wall, zones(wall, loc))
		if !p.at.After(p.start) {
			return nil, invalid(fmt.Sprintf("at 须晚于现在（现在是 %s，%s）", FormatLocal(p.start, loc, p.start), spec.TimeZone))
		}
		if !p.until.IsZero() && p.at.After(p.until) {
			return nil, invalid("at 不得晚于 until")
		}
		return p, nil
	}
	fields := make(map[string]string)
	for _, field := range strings.Split(spec.RRule, ";") {
		parts := strings.Split(field, "=")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, invalid("rrule 须由分号分隔的 键=值 组成")
		}
		key, value := strings.ToUpper(parts[0]), strings.ToUpper(parts[1])
		switch key {
		case "FREQ", "INTERVAL", "BYDAY", "BYMONTHDAY", "BYHOUR", "BYMINUTE", "COUNT", "UNTIL":
		default:
			return nil, invalid("rrule 只支持 FREQ、INTERVAL、BYDAY、BYMONTHDAY、BYHOUR、BYMINUTE、COUNT、UNTIL")
		}
		if _, exists := fields[key]; exists {
			return nil, invalid("rrule 的键不能重复")
		}
		fields[key] = value
	}
	p.freq = fields["FREQ"]
	if p.freq != "DAILY" && p.freq != "WEEKLY" && p.freq != "MONTHLY" {
		return nil, invalid("FREQ 须为 DAILY、WEEKLY 或 MONTHLY")
	}
	if fields["BYHOUR"] == "" || fields["BYMINUTE"] == "" {
		return nil, invalid("rrule 须带 BYHOUR 与 BYMINUTE，例如 FREQ=WEEKLY;BYDAY=FR;BYHOUR=17;BYMINUTE=0")
	}
	if fields["COUNT"] != "" && fields["UNTIL"] != "" {
		return nil, invalid("rrule 的 COUNT 与 UNTIL 不能同时填写")
	}
	if value := fields["INTERVAL"]; value != "" {
		p.interval, err = number(value, 1, 52)
		if err != nil {
			return nil, invalid("INTERVAL 须为 1–52 的整数")
		}
		fields["INTERVAL"] = strconv.Itoa(p.interval)
	}
	if value := fields["COUNT"]; value != "" {
		p.count, err = number(value, 1, 1000)
		if err != nil {
			return nil, invalid("COUNT 须为 1–1000 的整数")
		}
		fields["COUNT"] = strconv.Itoa(p.count)
	}
	if value := fields["UNTIL"]; value != "" {
		var until time.Time
		if strings.HasSuffix(value, "Z") {
			until, err = parseWall(value, "20060102T150405Z")
		} else {
			until, err = parseWall(value, "20060102T150405")
			if err == nil {
				until = resolve(until, zones(until, loc))
			}
		}
		if err != nil {
			return nil, invalid("UNTIL 须为 YYYYMMDDTHHMMSSZ（UTC）或 YYYYMMDDTHHMMSS（当地时间）")
		}
		if !until.After(p.start) {
			return nil, invalid("UNTIL 须晚于现在")
		}
		if p.until.IsZero() || until.Before(p.until) {
			p.until = until
		}
	}
	hours, err := numbers(fields["BYHOUR"], 0, 23, false)
	if err != nil {
		return nil, invalid("BYHOUR 须为 0–23 的整数，以逗号分隔")
	}
	minutes, err := numbers(fields["BYMINUTE"], 0, 59, false)
	if err != nil {
		return nil, invalid("BYMINUTE 须为 0–59 的整数，以逗号分隔")
	}
	fields["BYHOUR"] = joinNumbers(hours)
	fields["BYMINUTE"] = joinNumbers(minutes)
	for _, hour := range hours {
		for _, minute := range minutes {
			p.minutes = append(p.minutes, hour*60+minute)
		}
	}
	sort.Ints(p.minutes)
	for i, minute := range p.minutes {
		next := p.minutes[(i+1)%len(p.minutes)]
		if i == len(p.minutes)-1 {
			next += 24 * 60
		}
		if next-minute < 60 {
			return nil, invalid("两次执行至少间隔 1 小时（含跨午夜）")
		}
	}
	if value := fields["BYDAY"]; value != "" {
		canonical := make(map[string]bool)
		for _, token := range strings.Split(value, ",") {
			if len(token) < 2 {
				return nil, invalid("BYDAY 须为 MO、TU、WE、TH、FR、SA、SU；MONTHLY 可带 1–4 或 -1 序数")
			}
			suffix := token[len(token)-2:]
			index := -1
			for i, name := range []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"} {
				if suffix == name {
					index = i
				}
			}
			ordinal := 0
			prefix := token[:len(token)-2]
			if prefix != "" {
				ordinal, err = number(prefix, 1, 4)
				if prefix == "-1" {
					ordinal, err = -1, nil
				}
				if err != nil || p.freq != "MONTHLY" {
					return nil, invalid("BYDAY 序数只用于 MONTHLY，须为 1–4 或 -1")
				}
			}
			if index < 0 {
				return nil, invalid("BYDAY 的星期须为 MO、TU、WE、TH、FR、SA、SU")
			}
			name := suffix
			if ordinal != 0 {
				name = strconv.Itoa(ordinal) + suffix
			}
			if !canonical[name] {
				p.days = append(p.days, weekday{time.Weekday(index), ordinal})
				canonical[name] = true
			}
		}
		values := make([]string, 0, len(canonical))
		for value := range canonical {
			values = append(values, value)
		}
		sort.Strings(values)
		fields["BYDAY"] = strings.Join(values, ",")
	}
	if value := fields["BYMONTHDAY"]; value != "" {
		if p.freq != "MONTHLY" {
			return nil, invalid("BYMONTHDAY 只用于 MONTHLY")
		}
		p.monthDays, err = numbers(value, 1, 31, true)
		if err != nil {
			return nil, invalid("BYMONTHDAY 须为 1–31 或 -1，以逗号分隔")
		}
		fields["BYMONTHDAY"] = joinNumbers(p.monthDays)
	}
	if p.freq == "WEEKLY" && len(p.days) == 0 {
		p.days = []weekday{{p.anchor.Weekday(), 0}}
	}
	if p.freq == "MONTHLY" && len(p.days) == 0 && len(p.monthDays) == 0 {
		p.monthDays = []int{p.anchor.Day()}
	}
	// 稳定的键顺序与大小写方便持久化；列表中的重复值按集合解释。
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+fields[key])
	}
	p.spec.RRule = strings.Join(parts, ";")
	if !p.possible() {
		return nil, invalid("rrule 的周期与日期条件没有交集，请修改 INTERVAL、BYDAY 或 BYMONTHDAY")
	}
	if _, ok := p.Next(p.start); !ok {
		return nil, invalid("这个规则在十年内没有任何一次执行，请检查 BYMONTHDAY、BYDAY、INTERVAL 与 until/COUNT")
	}
	return p, nil
}

func number(s string, low, high int) (int, error) {
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrInvalid
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < low || n > high {
		return 0, ErrInvalid
	}
	return n, nil
}

func numbers(s string, low, high int, last bool) ([]int, error) {
	seen := make(map[int]bool)
	var result []int
	for _, token := range strings.Split(s, ",") {
		n, err := number(token, low, high)
		if last && token == "-1" {
			n, err = -1, nil
		}
		if err != nil {
			return nil, err
		}
		if !seen[n] {
			result = append(result, n)
			seen[n] = true
		}
	}
	sort.Ints(result)
	return result, nil
}

func parseWall(s string, layouts ...string) (time.Time, error) {
	for _, layout := range layouts {
		t, err := time.Parse(layout, s)
		// Parse 接受的某些宽松格式（如小数秒）不在声明契约中。
		if err == nil && t.Format(layout) == s {
			return t, nil
		}
	}
	return time.Time{}, ErrInvalid
}

func joinNumbers(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, ",")
}
