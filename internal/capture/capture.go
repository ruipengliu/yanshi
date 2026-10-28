// Package capture 渲染业务线返回的可信结构化旁听页（ADR-0050）。
package capture

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

const (
	// DefaultLimit 与 MaxLimit 是一页的段数。
	DefaultLimit = 200
	MaxLimit     = 500
	// MaxChars 保留原有字符数分页上限；整页另按调用方的 max_tool_result 预算切分。
	MaxChars = 6000
)

// Ref 是旁听引用的两个整数。在业务线命名空间中排序。
type Ref struct {
	Seq   uint64
	Index int
}

// String 是给模型看的引用编号的数字部分，如 "57.3"。
func (r Ref) String() string { return fmt.Sprintf("%d.%d", r.Seq, r.Index) }

// ParseRef 解析 "57.3"（引用编号去掉 "#"）。
func ParseRef(s string) (Ref, bool) {
	a, b, ok := strings.Cut(s, ".")
	seq, err1 := strconv.ParseUint(a, 10, 64)
	idx, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || idx < 0 || !session.ValidAttestedRef(s) {
		return Ref{}, false
	}
	return Ref{seq, idx}, true
}

// Cursor 是分页的位置：Last 是上一页读到的最后一段（零值表示从头读）；Span 非零时，上一页停在这个时段中间
// （段数或字数到了上限），下一页接着读同一个时段。
type Cursor struct {
	Last Ref
	Span Ref
	// 业务线允许 0.0，必须与内置游标的零值哨兵区分。
	lastSet, spanSet bool
}

// String 是给模型照抄的 after："57.3"，或停在时段中间时的 "57.3~50.0"。
func (c Cursor) String() string {
	if c.Last.Seq == 0 && !c.lastSet {
		return "0"
	}
	if c.Span.Seq != 0 || c.spanSet {
		return c.Last.String() + "~" + c.Span.String()
	}
	return c.Last.String()
}

var weekdays = [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

// dayHeader 注明当地日期与星期，并列出之后七天的日期与星期：模型自己推算星期常错一天（"下周一"写成 27 日），
// 对照着写就不必推算。
func dayHeader(t time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "—— %s %s（当地时间）——\n（之后七天：", t.Format(time.DateOnly), weekdays[t.Weekday()])
	for i := 1; i <= 7; i++ {
		d := t.AddDate(0, 0, i)
		if i > 1 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%d月%d日%s", d.Month(), d.Day(), weekdays[d.Weekday()])
	}
	b.WriteString("）\n")
	return b.String()
}

// segment 是业务线返回的一段旁听及其引用。
type segment struct {
	ref Ref
	g   *v1.CaptureSegment
}

func pageBudgetError(budget int) error {
	return fmt.Errorf("captures: context.max_tool_result=%d is too small for a complete Capture page; increase the AgentDef result budget", budget)
}

func pageTitle(span Ref, cont bool) string {
	title := "【时段 #" + span.String() + " 起】\n"
	if cont {
		title = "【时段 #" + span.String() + " 起（续）】\n"
	}
	return title
}

// pageTail 同时用于输出与预留，避免提示文字变长后预算漏算。
func pageTail(span, last Ref, prev *v1.CaptureSegment, count int, done, ended bool) (string, Cursor) {
	var tail string
	next := Cursor{Last: last, lastSet: last.Seq == 0}
	switch {
	case done:
		tail = fmt.Sprintf("（时段 #%s 起到此结束，本页 %d 段；已读完目前的全部旁听）", span, count)
	case ended:
		tail = fmt.Sprintf("（时段 #%s 起到此结束，本页 %d 段；下一时段用 after=\"%s\"）", span, count, next)
	default:
		// 没有时间的片段每页算一个时段：下一页不接着这段。
		if _, ok := model.CaptureLocalTime(prev); ok {
			next.Span, next.spanSet = span, span.Seq == 0
		}
		if next.Span.Seq != 0 || next.spanSet {
			tail = fmt.Sprintf("（时段 #%s 起未完，本页 %d 段；下一页用 after=\"%s\"）", span, count, next)
		} else {
			tail = fmt.Sprintf("（时段 #%s 起到此结束，本页 %d 段；下一时段用 after=\"%s\"）", span, count, next)
		}
	}
	return tail, next
}
