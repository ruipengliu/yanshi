package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"
)

// 本文件移植 internal/capture 的 page、spanTail、apart、游标和渲染计数，
// 以及 internal/model 的 CaptureText、CaptureLocalTime、EstimateTokens、TextBlocks。
// ADR-0043/Q31 禁止 Persona import 内核；差分测试守住线上格式与切页口径。
const (
	DefaultCaptureLimit = 200
	MaxCaptureLimit     = 500
)

// PageOptions 是平台经 MCP 传入的分页参数。
type PageOptions struct {
	Limit         int   `json:"limit"`
	MaxChars      int   `json:"max_chars"`
	GapMS         int64 `json:"gap_ms"`
	MaxToolResult int   `json:"max_tool_result"`
}

// StructuredPage 只交出原文与页的事实，页头、页尾和游标由平台生成。
type StructuredPage struct {
	Kind     string        `json:"kind"`
	Span     string        `json:"span,omitempty"`
	Cont     bool          `json:"cont,omitempty"`
	Done     bool          `json:"done,omitempty"`
	Ended    bool          `json:"ended,omitempty"`
	Segments []PageSegment `json:"segments,omitempty"`
	LastAt   string        `json:"last_at,omitempty"`
	TimeZone string        `json:"time_zone,omitempty"`
}

// PageSegment 在提交片段上附业务日中的引用，不允许调用方填写审核标记。
type PageSegment struct {
	Ref        string `json:"ref"`
	Speaker    string `json:"speaker"`
	SpeakerKey string `json:"speaker_key,omitempty"`
	Text       string `json:"text"`
	SpokenAt   string `json:"spoken_at,omitempty"`
	TimeZone   string `json:"time_zone,omitempty"`
}

type captureCursor struct {
	Last Ref
	Span Ref
}

// String 是给模型照抄的 after："57.3"，或停在时段中间时的 "57.3~50.0"。
func (c captureCursor) String() string {
	if c.Last.Seq == 0 {
		return "0"
	}
	if c.Span.Seq != 0 {
		return c.Last.String() + "~" + c.Span.String()
	}
	return c.Last.String()
}

// parseCaptureCursor 解析 after，字符串形式见 captureCursor.String；空串与 "0" 表示从头读。
func parseCaptureCursor(s string) (captureCursor, error) {
	if s == "" || s == "0" {
		return captureCursor{}, nil
	}
	last, ex, cont := strings.Cut(s, "~")
	c := captureCursor{}
	var ok bool
	if c.Last, ok = parseCaptureRef(last); !ok || c.Last.Seq == 0 {
		return captureCursor{}, fmt.Errorf("invalid arguments: after %q is not a position given by a previous page", s)
	}
	if cont {
		if c.Span, ok = parseCaptureRef(ex); !ok || c.Span.Seq == 0 || c.Span.Seq > c.Last.Seq || (c.Span.Seq == c.Last.Seq && c.Span.Index > c.Last.Index) {
			return captureCursor{}, fmt.Errorf("invalid arguments: after %q is not a position given by a previous page", s)
		}
	}
	return c, nil
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
func pageTail(span, last Ref, prev CaptureSegment, count int, done, ended bool) (string, captureCursor) {
	var tail string
	next := captureCursor{Last: last}
	switch {
	case done:
		tail = fmt.Sprintf("（时段 #%s 起到此结束，本页 %d 段；已读完目前的全部旁听）", span, count)
	case ended:
		tail = fmt.Sprintf("（时段 #%s 起到此结束，本页 %d 段；下一时段用 after=\"%s\"）", span, count, next)
	default:
		// 没有时间的片段每页算一个时段：下一页不接着这段。
		if _, ok := captureLocalTime(prev); ok {
			next.Span = span
		}
		if next.Span.Seq != 0 {
			tail = fmt.Sprintf("（时段 #%s 起未完，本页 %d 段；下一页用 after=\"%s\"）", span, count, next)
		} else {
			tail = fmt.Sprintf("（时段 #%s 起到此结束，本页 %d 段；下一时段用 after=\"%s\"）", span, count, next)
		}
	}
	return tail, next
}

// parseCaptureRef 额外限制纯数字，避免已有领域 ParseRef 接受带正号的整数。
func parseCaptureRef(s string) (Ref, bool) {
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return Ref{}, false
		}
	}
	return ParseRef(s)
}

func captureZone(name string) (*time.Location, bool) {
	if name == "" || name == "Local" || len(name) > 64 {
		return nil, false
	}
	zone, err := time.LoadLocation(name)
	return zone, err == nil
}

func captureLocalTime(g CaptureSegment) (time.Time, bool) {
	if g.SpokenAt == "" {
		return time.Time{}, false
	}
	zone, ok := captureZone(g.TimeZone)
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, g.SpokenAt)
	return at.In(zone), err == nil
}

func captureText(g CaptureSegment) string {
	who := "佩戴者"
	if g.Speaker == "other" {
		who = "他人 " + g.SpeakerKey
	}
	at := ""
	if t, ok := captureLocalTime(g); ok {
		at = t.Format("15:04") + " "
	}
	return at + "[" + who + "] " + strings.NewReplacer("\r", " ", "\n", " ").Replace(g.Text) + "\n"
}

func captureApart(a, b CaptureSegment, gap time.Duration) bool {
	ta, ok1 := captureLocalTime(a)
	tb, ok2 := captureLocalTime(b)
	return ok1 && ok2 && tb.Sub(ta) > gap
}

type captureSegment struct {
	ref Ref
	g   CaptureSegment
}

// estimateCaptureBlocks 分块取整；先拼字符串再估算会改变预算切点。
func estimateCaptureBlocks(blocks ...string) int {
	n := 0
	for _, b := range blocks {
		n += (len(b) + 2) / 3
	}
	return n
}

func captureSegmentBlocks(s captureSegment, day string) ([]string, string) {
	var blocks []string
	if t, ok := captureLocalTime(s.g); ok && t.Format(time.DateOnly) != day {
		day = t.Format(time.DateOnly)
		blocks = append(blocks, dayHeader(t))
	}
	return append(blocks, "#"+s.ref.String()+" ", captureText(s.g)), day
}

// Captures 的正式读取必须在 Update 内调用：固定时段间隔，并记下当前工作 Session 实际返回过的页尾。
// 夹具预览只在 View 的隔离快照上调用，不提交这些变化。
func (c *Call) Captures(o PageOptions) (StructuredPage, error) {
	_, day := c.u.boundDay(c.session)
	if day == nil {
		return StructuredPage{}, fmt.Errorf("这个 Session 没有绑定业务日")
	}
	var in struct {
		After string `json:"after"`
		All   bool   `json:"all"`
		Limit int    `json:"limit"`
	}
	if len(c.args) > 0 {
		if err := json.Unmarshal(c.args, &in); err != nil {
			return StructuredPage{}, fmt.Errorf("invalid arguments: after must be a string such as \"0\", \"57.3\" or \"new\"")
		}
	}
	if in.After == "new" {
		in.After = day.Progress
	}
	at, err := parseCaptureCursor(in.After)
	if err != nil {
		return StructuredPage{}, err
	}
	if in.Limit <= 0 {
		in.Limit = DefaultCaptureLimit
	}
	// 模型参数决定实际页大小，平台给出可接受的段数上限。
	o.Limit = min(in.Limit, MaxCaptureLimit, o.Limit)
	if !day.GapSet {
		day.GapMS, day.GapSet = o.GapMS, true
	}
	o.GapMS = day.GapMS
	page, err := capturePage(day.segments(), at, o, c.now, in.All)
	if err == nil && page.Kind == "page" && len(page.Segments) > 0 {
		last, _ := parseCaptureRef(page.Segments[len(page.Segments)-1].Ref)
		if refLE(day.ReadThrough, last) {
			day.ReadThrough = last
		}
	}
	return page, err
}

// capturePage 与内核 page、ParsePage 的字符计数顺序一致：先检查下一段，再加入新日期头。
func capturePage(segs []captureSegment, at captureCursor, o PageOptions, now time.Time, all bool) (out StructuredPage, err error) {
	defer func() {
		if err == nil && o.MaxToolResult > 0 && capturePageTokens(out) > o.MaxToolResult {
			out, err = StructuredPage{}, pageBudgetError(o.MaxToolResult)
		}
	}()
	gap := time.Duration(o.GapMS) * time.Millisecond
	var prev CaptureSegment
	havePrev := false
	span := Ref{}
	count, chars, tokens := 0, 0, 0
	day := ""
	ended, done, cont := false, true, false
	nextIndex := len(segs)
	for i, s := range segs {
		// 内核从上一批起读，前一批的末段不参与续页判断。
		if s.ref.Seq < at.Last.Seq {
			continue
		}
		if refLE(s.ref, at.Last) {
			prev, havePrev = s.g, true
			continue
		}
		n := utf8.RuneCountInString("#" + s.ref.String() + " " + captureText(s.g))
		if count == 0 {
			span = s.ref
			if at.Span.Seq != 0 && havePrev && !captureApart(prev, s.g, gap) {
				span, cont = at.Span, true
			}
		} else {
			if captureApart(prev, s.g, gap) {
				ended, done, nextIndex = true, false, i
				break
			}
			if count+1 > o.Limit || chars+n > o.MaxChars {
				done, nextIndex = false, i
				break
			}
		}
		added, newDay := captureSegmentBlocks(s, day)
		headerChars := 0
		if newDay != day {
			headerChars = utf8.RuneCountInString(added[0])
		}
		addedTokens := estimateCaptureBlocks(added...)
		if o.MaxToolResult > 0 {
			reserved := 0
			for _, state := range [][2]bool{{true, false}, {false, true}, {false, false}} {
				tail, _ := pageTail(span, s.ref, s.g, count+1, state[0], state[1])
				reserved = max(reserved, estimateCaptureBlocks(tail))
			}
			if estimateCaptureBlocks(pageTitle(span, cont))+tokens+addedTokens+reserved > o.MaxToolResult {
				if count == 0 {
					return StructuredPage{}, pageBudgetError(o.MaxToolResult)
				}
				done, nextIndex = false, i
				break
			}
		}
		out.Segments = append(out.Segments, PageSegment{Ref: s.ref.String(), Speaker: s.g.Speaker, SpeakerKey: s.g.SpeakerKey, Text: s.g.Text, SpokenAt: s.g.SpokenAt, TimeZone: s.g.TimeZone})
		tokens += addedTokens
		chars += headerChars + n
		day = newDay
		count++
		prev, havePrev = s.g, true
	}
	if count == 0 {
		return StructuredPage{Kind: "empty"}, nil
	}
	lastSeg, atEnd := prev, done
	if !done && !ended && !cont && !all && !now.IsZero() {
		lastSeg, atEnd = captureSpanTail(segs[nextIndex:], prev, gap)
	}
	if t, ok := captureLocalTime(lastSeg); atEnd && ok && !all && !now.IsZero() && now.Sub(t) < gap {
		return StructuredPage{Kind: "in_progress", Span: span.String(), LastAt: t.Format(time.RFC3339Nano), TimeZone: lastSeg.TimeZone}, nil
	}
	out.Kind, out.Span, out.Cont, out.Done, out.Ended = "page", span.String(), cont, done, ended
	return out, nil
}

func captureSpanTail(segs []captureSegment, prev CaptureSegment, gap time.Duration) (CaptureSegment, bool) {
	for _, s := range segs {
		if captureApart(prev, s.g, gap) {
			return prev, false
		}
		prev = s.g
	}
	return prev, true
}

func capturePageTokens(p StructuredPage) int {
	if p.Kind == "empty" {
		return estimateCaptureBlocks("（已读完：之后没有更多旁听）")
	}
	span, _ := parseCaptureRef(p.Span)
	if p.Kind == "in_progress" {
		at, _ := time.Parse(time.RFC3339Nano, p.LastAt)
		return estimateCaptureBlocks(fmt.Sprintf("（时段 #%s 还在进行：最后一段在 %s，稍后再整理。之前的时段已读完）", span, at.Format("15:04")))
	}
	n, day := estimateCaptureBlocks(pageTitle(span, p.Cont)), ""
	var last captureSegment
	for _, g := range p.Segments {
		ref, _ := parseCaptureRef(g.Ref)
		last = captureSegment{ref: ref, g: CaptureSegment{Speaker: g.Speaker, SpeakerKey: g.SpeakerKey, Text: g.Text, SpokenAt: g.SpokenAt, TimeZone: g.TimeZone}}
		blocks, nextDay := captureSegmentBlocks(last, day)
		n += estimateCaptureBlocks(blocks...)
		day = nextDay
	}
	tail, _ := pageTail(span, last.ref, last.g, len(p.Segments), p.Done, p.Ended)
	return n + estimateCaptureBlocks(tail)
}
