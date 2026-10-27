package domain

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 期限的换算与健康信息的检查是业务线按规则做的确定性检查（xinli-model.md §2 原则 5、§5）：模型只做判断，
// 日期由业务线按被引用原话的说话那天换算核对。

var (
	// dueRE 匹配期限中能确定日期的说法；一段说法中出现几处（"周一到周三"）时每处都算一个可接受的日期。
	dueRE = regexp.MustCompile(`大后天|后天|明天|明早|明晚|明日|今天|今晚|今早|今日|当天|月底|` +
		`(下下|下个|下|这个|这|本)?(?:周|星期|礼拜)([一二三四五六日天])|` +
		`([0-9]{1,2}|[一二三四五六七八九十]{1,3})月([0-9]{1,2}|[一二三四五六七八九十]{1,3})[日号]|` +
		`([0-9]{1,2}|[一二三四五六七八九十]{1,3})[日号]`)
	weekdayOf = map[string]int{"一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6, "日": 7, "天": 7}
)

// dueDates 返回按说话时刻 at（当地时间）把期限的说法 text 换算出的日期（YYYY-MM-DD，按出现的顺序）；换算不了时为 nil。
// 期限总是将来：单说"周三"指之后最近的周三（说话当天也是周三时两天都接受）；只说几号时，已过的指下个月。
func dueDates(text string, at time.Time) []string {
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	wd := int(day.Weekday())
	if wd == 0 {
		wd = 7
	}
	monday := day.AddDate(0, 0, 1-wd)
	var out []time.Time
	for _, m := range dueRE.FindAllStringSubmatchIndex(text, -1) {
		g := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return text[m[2*i]:m[2*i+1]]
		}
		switch s := g(0); {
		case s == "大后天":
			out = append(out, day.AddDate(0, 0, 3))
		case s == "后天":
			out = append(out, day.AddDate(0, 0, 2))
		case strings.HasPrefix(s, "明"):
			out = append(out, day.AddDate(0, 0, 1))
		case strings.HasPrefix(s, "今") || s == "当天":
			out = append(out, day)
		case s == "月底":
			out = append(out, time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, day.Location()))
		case g(2) != "":
			target := weekdayOf[g(2)]
			switch g(1) {
			case "下下":
				out = append(out, monday.AddDate(0, 0, 14+target-1))
			case "下", "下个":
				out = append(out, monday.AddDate(0, 0, 7+target-1))
			case "这", "这个", "本":
				out = append(out, monday.AddDate(0, 0, target-1))
			default:
				if target == wd {
					out = append(out, day)
				}
				out = append(out, day.AddDate(0, 0, (target-wd+6)%7+1))
			}
		case g(3) != "":
			mo, d := chineseNumber(g(3)), chineseNumber(g(4))
			if mo < 1 || mo > 12 || d < 1 || d > 31 {
				continue
			}
			t := time.Date(day.Year(), time.Month(mo), d, 0, 0, 0, 0, day.Location())
			if t.Before(day) {
				t = t.AddDate(1, 0, 0)
			}
			out = append(out, t)
		case g(5) != "":
			d := chineseNumber(g(5))
			if d < 1 || d > 31 {
				continue
			}
			t := time.Date(day.Year(), day.Month(), d, 0, 0, 0, 0, day.Location())
			if d < day.Day() {
				t = time.Date(day.Year(), day.Month()+1, d, 0, 0, 0, 0, day.Location())
			}
			out = append(out, t)
		}
	}
	var dates []string
	for _, t := range out {
		if s := t.Format(time.DateOnly); !slices.Contains(dates, s) {
			dates = append(dates, s)
		}
	}
	return dates
}

// chineseNumber 解析 1–31 的阿拉伯数字或中文数字（"十一""二十八"），解析不了时为 0。
func chineseNumber(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	digit := map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	r := []rune(s)
	switch {
	case len(r) == 1 && r[0] == '十':
		return 10
	case len(r) == 1:
		return digit[r[0]]
	case len(r) == 2 && r[0] == '十':
		return 10 + digit[r[1]]
	case len(r) == 2 && r[1] == '十':
		return digit[r[0]] * 10
	case len(r) == 3 && r[1] == '十':
		return digit[r[0]]*10 + digit[r[2]]
	}
	return 0
}

// healthWords 是业务线兜底检查的健康信息用词（F3）：只收明确指身心健康的词。"情绪""压力"不收：前者在评价他人的
// 工作态度时常用，后者有"压力测试"；指令仍须写明不写这类内容。
var healthWords = []string{"失眠", "睡不着", "睡不好", "血压", "降压", "血糖", "胃病", "胃不好", "胃疼", "胃痛", "胃受不了",
	"焦虑", "抑郁", "心理咨询", "吃药", "住院", "看病", "体检", "头疼", "头痛", "发烧", "感冒", "生病", "医生"}

// healthWord 返回 s 中第一个健康信息用词，没有时为空。
func healthWord(s string) string {
	if ws := healthIn(s); len(ws) > 0 {
		return ws[0]
	}
	return ""
}

// healthIn 返回 texts 中出现的全部健康信息用词：一次说全，模型一次改完，而不是删一个、再被下一个拒绝。
func healthIn(texts ...string) []string {
	var out []string
	for _, s := range texts {
		for _, w := range healthWords {
			if strings.Contains(s, w) && !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	return out
}
