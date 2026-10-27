package xinlimock

import (
	"slices"
	"testing"
	"time"
)

// 期限按说话那天换算（xinli-model.md §7）：相对的说法换成日期，"下周"从下一个星期一算起；单说"周三"指之后
// 最近的周三，说话当天也是周三时两天都接受；换算不了的不检查。
func TestDueDates(t *testing.T) {
	fri := time.Date(2026, 10, 23, 10, 40, 0, 0, time.Local) // 星期五
	sun := time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local)   // 星期日
	for _, c := range []struct {
		text string
		at   time.Time
		want []string
	}{
		{"明天上午", fri, []string{"2026-10-24"}},
		{"今晚", fri, []string{"2026-10-23"}},
		{"后天", fri, []string{"2026-10-25"}},
		{"下周二", fri, []string{"2026-10-27"}},
		{"下周三前", fri, []string{"2026-10-28"}},
		{"下个星期一", fri, []string{"2026-10-26"}},
		{"下周二", sun, []string{"2026-10-27"}},
		{"周日", fri, []string{"2026-10-25"}},
		{"周五", fri, []string{"2026-10-23", "2026-10-30"}},
		{"这周六", fri, []string{"2026-10-24"}},
		{"下下周一", fri, []string{"2026-11-02"}},
		{"11月2日", fri, []string{"2026-11-02"}},
		{"十一月二号", fri, []string{"2026-11-02"}},
		{"1月5号", fri, []string{"2027-01-05"}},
		{"28号", fri, []string{"2026-10-28"}},
		{"3号", fri, []string{"2026-11-03"}},
		{"周一到周三", fri, []string{"2026-10-26", "2026-10-28"}},
		{"月底", fri, []string{"2026-10-31"}},
		{"尽快", fri, nil},
		{"周末", fri, nil},
	} {
		if got := dueDates(c.text, c.at); !slices.Equal(got, c.want) {
			t.Errorf("dueDates(%q, %s) = %v, want %v", c.text, c.at.Format("01-02 Mon"), got, c.want)
		}
	}
}

// 健康信息（包括他人的）不写进交谈、决定、风险、觉察与项目名（xinli-model.md §5）。
func TestHealthWords(t *testing.T) {
	if w := healthWord("老陈说最近失眠，想调岗"); w != "失眠" {
		t.Fatalf("healthWord = %q", w)
	}
	for _, s := range []string{"康宁大药房的排班系统", "压力测试下周做完", "老陈对调岗有情绪"} {
		if w := healthWord(s); w != "" {
			t.Fatalf("healthWord(%q) = %q", s, w)
		}
	}
}
