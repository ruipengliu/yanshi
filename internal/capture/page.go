package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// PageOptions 由平台交给业务线，避免两边的分页与结果预算不一致。
type PageOptions struct {
	Limit         int   `json:"limit"`
	MaxChars      int   `json:"max_chars"`
	GapMS         int64 `json:"gap_ms"`
	MaxToolResult int   `json:"max_tool_result"`
}

func OptionsFor(inv capability.Invocation) PageOptions {
	gap := invocationGap(inv)
	o := PageOptions{Limit: DefaultLimit, MaxChars: MaxChars, GapMS: gap.Milliseconds()}
	if inv.Agent != nil {
		o.MaxToolResult = inv.Agent.Context.MaxToolResult
	}
	return o
}

// invocationGap 保留内置首次读取的 duration 精度，线上参数另转换为毫秒。
func invocationGap(inv capability.Invocation) time.Duration {
	gap := agentdef.DefaultExchangeGap
	if d, ok := inv.Agent.Stream(Stream); ok {
		gap = d.ExchangeGap
	}
	if inv.State != nil {
		if r := inv.State.Captures.Read[Stream]; r != nil {
			gap = time.Duration(r.GetExchangeGapMs()) * time.Millisecond
		}
	}
	return gap
}

// StructuredPage 是可信 MCP 页的线上契约；不给业务线提供填写页头或游标的入口。
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

type PageSegment struct {
	Ref        string `json:"ref"`
	Speaker    string `json:"speaker"`
	SpeakerKey string `json:"speaker_key,omitempty"`
	Text       string `json:"text"`
	SpokenAt   string `json:"spoken_at,omitempty"`
	TimeZone   string `json:"time_zone,omitempty"`
}

// RenderPage 是校验后的页。片段只在 Worker 审核时改成占位，业务线不能自报已审核。
type RenderPage struct {
	Kind              string
	Span              Ref
	Cont, Done, Ended bool
	Segments          []*v1.CaptureSegment
	LastAt            time.Time
}

func invalidPage(reason string) error {
	return fmt.Errorf("业务线返回的旁听页无效：%s", reason)
}

func refBefore(a, b Ref) bool { return a.Seq < b.Seq || a.Seq == b.Seq && a.Index < b.Index }

// ParsePage 严格解析并校验，错误只包含平台固定类别，不能回显未审核内容。
func ParsePage(raw json.RawMessage, o PageOptions) (RenderPage, error) {
	var wire StructuredPage
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&wire) != nil || d.Decode(new(any)) != io.EOF {
		return RenderPage{}, invalidPage("格式")
	}
	p := RenderPage{Kind: wire.Kind, Cont: wire.Cont, Done: wire.Done, Ended: wire.Ended}
	if wire.Kind != "page" && wire.Kind != "empty" && wire.Kind != "in_progress" {
		return p, invalidPage("种类")
	}
	if wire.Kind != "page" && len(wire.Segments) != 0 {
		return p, invalidPage("段数")
	}
	if wire.TimeZone != "" {
		if _, ok := model.CaptureZone(wire.TimeZone); !ok {
			return p, invalidPage("时区")
		}
	}
	if wire.LastAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, wire.LastAt); err != nil {
			return p, invalidPage("末段时间")
		}
	}
	if wire.Kind == "empty" {
		return p, nil
	}
	span, ok := ParseRef(wire.Span)
	if !ok {
		return p, invalidPage("时段引用")
	}
	p.Span = span
	if wire.Kind == "in_progress" {
		at, err := time.Parse(time.RFC3339Nano, wire.LastAt)
		if err != nil {
			return p, invalidPage("末段时间")
		}
		// 无额外时区时 RFC3339 的显式偏移即末段当地时间。
		if wire.TimeZone != "" {
			zone, ok := model.CaptureZone(wire.TimeZone)
			if !ok {
				return p, invalidPage("时区")
			}
			at = at.In(zone)
		}
		p.LastAt = at
		return p, nil
	}
	if len(wire.Segments) == 0 || len(wire.Segments) > o.Limit {
		return p, invalidPage("段数")
	}
	var prev Ref
	day, chars := "", 0
	for i, s := range wire.Segments {
		ref, ok := ParseRef(s.Ref)
		if !ok || i > 0 && !refBefore(prev, ref) {
			return p, invalidPage("片段引用")
		}
		if i == 0 && (refBefore(ref, span) || wire.Cont && !refBefore(span, ref)) {
			return p, invalidPage("时段引用")
		}
		prev = ref
		g := &v1.CaptureSegment{Ref: ref.String(), Text: s.Text, SpeakerKey: s.SpeakerKey, TimeZone: s.TimeZone}
		switch s.Speaker {
		case "wearer":
			g.Speaker = v1.Speaker_SPEAKER_WEARER
			if s.SpeakerKey != "" {
				return p, invalidPage("说话人键")
			}
		case "other":
			g.Speaker = v1.Speaker_SPEAKER_OTHER
			if !session.ValidSpeakerKey(s.SpeakerKey) {
				return p, invalidPage("说话人键")
			}
		default:
			return p, invalidPage("说话人")
		}
		if s.Text == "" {
			return p, invalidPage("片段文字")
		}
		if s.SpokenAt != "" {
			at, err := time.Parse(time.RFC3339Nano, s.SpokenAt)
			if err != nil {
				return p, invalidPage("说话时间")
			}
			g.SpokenAt = timestamppb.New(at)
			if g.SpokenAt.CheckValid() != nil {
				return p, invalidPage("说话时间")
			}
		}
		if s.TimeZone != "" {
			if _, ok := model.CaptureZone(s.TimeZone); !ok {
				return p, invalidPage("时区")
			}
		}
		blocks, nextDay := renderSegment(segment{ref: ref, g: g}, day)
		day = nextDay
		for _, b := range blocks {
			if b.GetCapture() != nil {
				chars += utf8.RuneCountInString(model.CaptureText(g))
			} else {
				chars += utf8.RuneCountInString(b.GetText().GetText())
			}
		}
		if chars > o.MaxChars {
			return p, invalidPage("字符上限")
		}
		p.Segments = append(p.Segments, g)
	}
	return p, nil
}

// Render 与日志读取共用同一纯渲染器，预算检查在审核之后也执行，防止占位比原话长。
func Render(p RenderPage, maxToolResult int) ([]*v1.ContentBlock, Cursor, error) {
	segs := make([]segment, len(p.Segments))
	for i, g := range p.Segments {
		ref, ok := ParseRef(g.GetRef())
		if !ok {
			return nil, Cursor{}, invalidPage("片段引用")
		}
		segs[i] = segment{ref: ref, g: g}
	}
	blocks, next := renderPage(p.Kind, p.Span, p.Cont, p.Done, p.Ended, segs, p.LastAt)
	if maxToolResult > 0 && model.EstimateTokens(blocks) > maxToolResult {
		return nil, Cursor{}, pageBudgetError(maxToolResult)
	}
	return blocks, next, nil
}

func renderSegment(s segment, day string) ([]*v1.ContentBlock, string) {
	var out []*v1.ContentBlock
	if t, ok := model.CaptureLocalTime(s.g); ok && t.Format(time.DateOnly) != day {
		day = t.Format(time.DateOnly)
		out = append(out, model.TextBlocks(dayHeader(t))...)
	}
	out = append(out, model.TextBlocks("#"+s.ref.String()+" ")...)
	out = append(out, &v1.ContentBlock{Kind: &v1.ContentBlock_Capture{Capture: s.g}})
	return out, day
}

func renderPage(kind string, span Ref, cont, done, ended bool, segs []segment, lastAt time.Time) ([]*v1.ContentBlock, Cursor) {
	switch kind {
	case "empty":
		return model.TextBlocks("（已读完：之后没有更多旁听）"), Cursor{}
	case "in_progress":
		return model.TextBlocks(fmt.Sprintf("（时段 #%s 还在进行：最后一段在 %s，稍后再整理。之前的时段已读完）", span, lastAt.Format("15:04"))), Cursor{}
	}
	out := model.TextBlocks(pageTitle(span, cont))
	day := ""
	for _, s := range segs {
		added, nextDay := renderSegment(s, day)
		day = nextDay
		out = append(out, added...)
	}
	last := segs[len(segs)-1]
	tail, next := pageTail(span, last.ref, last.g, len(segs), done, ended)
	return append(out, model.TextBlocks(tail)...), next
}
