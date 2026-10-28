package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

var captureTestNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func captureTestSegment(at time.Time) CaptureSegment {
	return CaptureSegment{Speaker: "wearer", Text: "确定周五交付。", SpokenAt: at.Format(time.RFC3339Nano), TimeZone: "UTC"}
}
func captureTestOptions() PageOptions {
	return PageOptions{Limit: 500, MaxChars: 6000, GapMS: 300000, MaxToolResult: 8000}
}
func captureTestBook(t *testing.T, segs []CaptureSegment) *Book {
	t.Helper()
	b := NewBook()
	if err := b.BindDay("2026-10-27", "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AppendCaptures("2026-10-27", "one", segs); err != nil {
		t.Fatal(err)
	}
	return b
}
func captureTestRead(t *testing.T, b *Book, args string, o PageOptions) StructuredPage {
	t.Helper()
	p, err := NewCall(b, "s", json.RawMessage(args), nil, captureTestNow).Captures(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func captureTestAfter(p StructuredPage) string {
	span, _ := parseCaptureRef(p.Span)
	s := p.Segments[len(p.Segments)-1]
	last, _ := parseCaptureRef(s.Ref)
	_, next := pageTail(span, last, CaptureSegment{SpokenAt: s.SpokenAt, TimeZone: s.TimeZone}, len(p.Segments), p.Done, p.Ended)
	return next.String()
}

func TestAppendCaptures(t *testing.T) {
	b := NewBook()
	segs := []CaptureSegment{captureTestSegment(captureTestNow), {Speaker: "other", SpeakerKey: "other-1", Text: "好的"}}
	first, err := b.AppendCaptures("2026-10-27", "a", segs)
	if err != nil || !reflect.DeepEqual(first, []string{"1.0", "1.1"}) {
		t.Fatalf("refs %v: %v", first, err)
	}
	again, err := b.AppendCaptures("2026-10-27", "a", segs)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("duplicate %v: %v", again, err)
	}
	changed := append([]CaptureSegment(nil), segs...)
	changed[0].Text = "different"
	if _, err := b.AppendCaptures("2026-10-27", "a", changed); err == nil {
		t.Fatal("different duplicate accepted")
	}
	refs, err := b.AppendCaptures("2026-10-27", "b", segs[:1])
	if err != nil || refs[0] != "2.0" {
		t.Fatalf("next %v: %v", refs, err)
	}
	refs, err = b.AppendCaptures("2026-10-28", "a", segs[:1])
	if err != nil || refs[0] != "1.0" {
		t.Fatalf("next day %v: %v", refs, err)
	}
	// 输入、返回引用和存储快照均不能变成事务外写入的后门。
	segs[0].Text = "mutated"
	first[0] = "999.0"
	clone, err := b.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if err := clone.BindDay("2026-10-27", "s"); err != nil {
		t.Fatal(err)
	}
	p := captureTestRead(t, clone, `{"all":true}`, captureTestOptions())
	if p.Segments[0].Text == "mutated" || p.Segments[0].Ref != "1.0" {
		t.Fatal("aliased capture")
	}
	clone.captureDays["2026-10-27"].Batches[0].Segments[0].Text = "clone"
	if b.captureDays["2026-10-27"].Batches[0].Segments[0].Text == "clone" {
		t.Fatal("aliased clone")
	}
}

func TestCaptureValidation(t *testing.T) {
	valid := captureTestSegment(captureTestNow)
	for name, change := range map[string]func(*CaptureSegment){
		"speaker":           func(g *CaptureSegment) { g.Speaker = "unknown" },
		"wearer key":        func(g *CaptureSegment) { g.SpeakerKey = "x" },
		"other key missing": func(g *CaptureSegment) { g.Speaker = "other" },
		"other key invalid": func(g *CaptureSegment) { g.Speaker = "other"; g.SpeakerKey = "bad key" },
		"other key long":    func(g *CaptureSegment) { g.Speaker = "other"; g.SpeakerKey = strings.Repeat("x", 33) },
		"empty":             func(g *CaptureSegment) { g.Text = "" },
		"long":              func(g *CaptureSegment) { g.Text = strings.Repeat("字", 2001) },
		"time":              func(g *CaptureSegment) { g.SpokenAt = "bad" },
		"year zero":         func(g *CaptureSegment) { g.SpokenAt = "0000-01-01T00:00:00Z" },
		"zone":              func(g *CaptureSegment) { g.TimeZone = "bad-zone" },
		"local":             func(g *CaptureSegment) { g.TimeZone = "Local" },
	} {
		t.Run(name, func(t *testing.T) {
			g := valid
			change(&g)
			b := NewBook()
			if _, err := b.AppendCaptures("2026-10-27", "a", []CaptureSegment{g}); err == nil {
				t.Fatal("accepted")
			}
			if len(b.captureDays) != 0 {
				t.Fatal("invalid append changed book")
			}
		})
	}
	b := NewBook()
	for _, id := range []string{"", strings.Repeat("i", 129)} {
		if _, err := b.AppendCaptures("2026-10-27", id, []CaptureSegment{valid}); err == nil {
			t.Fatal("invalid id")
		}
	}
	for _, n := range []int{0, 201} {
		if _, err := b.AppendCaptures("2026-10-27", "a", make([]CaptureSegment, n)); err == nil {
			t.Fatal("invalid count")
		}
	}
	if _, err := b.AppendCaptures("2026-02-30", "a", []CaptureSegment{valid}); err == nil {
		t.Fatal("invalid date")
	}
	valid.Text = strings.Repeat("字", 2000)
	valid.SpokenAt = ""
	valid.TimeZone = ""
	if _, err := b.AppendCaptures("2026-10-27", strings.Repeat("i", 128), []CaptureSegment{valid}); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureCursor(t *testing.T) {
	for _, s := range []string{"", "0", "57.3", "57.3~50.0", "18446744073709551615.0"} {
		c, err := parseCaptureCursor(s)
		if err != nil {
			t.Fatal(s, err)
		}
		want := s
		if want == "" {
			want = "0"
		}
		if c.String() != want {
			t.Fatal(s, c.String())
		}
	}
	for _, s := range []string{"#1.0", "0.0", "-1.0", "+1.0", "1.+0", "1.-1", "1.0~2.0", "1.0~1.1", "1.0~0.0", "1.0~1.0~1.0", "18446744073709551616.0", "1.99999999999999999999"} {
		if _, err := parseCaptureCursor(s); err == nil || !strings.HasPrefix(err.Error(), "invalid arguments: after ") {
			t.Fatalf("invalid %q: %v", s, err)
		}
	}
}

func TestCapturePages(t *testing.T) {
	old := captureTestNow.Add(-time.Hour)
	segs := []CaptureSegment{captureTestSegment(old), captureTestSegment(old.Add(time.Minute)), captureTestSegment(old.Add(2 * time.Minute)), captureTestSegment(old.Add(20 * time.Minute))}
	b := captureTestBook(t, segs)
	o := captureTestOptions()
	p := captureTestRead(t, b, `{"limit":2}`, o)
	if len(p.Segments) != 2 || p.Done || p.Ended || captureTestAfter(p) != "1.1~1.0" {
		t.Fatalf("first %+v", p)
	}
	p = captureTestRead(t, b, `{"after":"1.1~1.0","limit":2}`, o)
	if !p.Cont || !p.Ended || len(p.Segments) != 1 || captureTestAfter(p) != "1.2" {
		t.Fatalf("continued %+v", p)
	}
	p = captureTestRead(t, b, `{"after":"1.2"}`, o)
	if p.Cont || !p.Done || p.Span != "1.3" {
		t.Fatalf("last %+v", p)
	}
	p = captureTestRead(t, b, `{"after":"1.3"}`, o)
	if p.Kind != "empty" {
		t.Fatalf("empty %+v", p)
	}
	// 首读固定间隔，克隆模拟跨事务；修改后续平台参数不能合并原来的两个时段。
	clone, err := b.Clone()
	if err != nil {
		t.Fatal(err)
	}
	o.GapMS = 3600000
	p = captureTestRead(t, clone, `{}`, o)
	if !p.Ended || len(p.Segments) != 3 {
		t.Fatalf("gap changed %+v", p)
	}
	// 新 Session 失去旧 Session 的读权限，业务日的数据和进度保留。
	if err := b.BindDay("2026-10-27", "replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCall(b, "s", nil, nil, captureTestNow).Captures(o); err == nil || err.Error() != "这个 Session 没有绑定业务日" {
		t.Fatal(err)
	}
	if err := b.BindDay("2026-10-28", "replacement"); err == nil {
		t.Fatal("ambiguous binding")
	}
}

func TestCaptureInProgressAndTimeless(t *testing.T) {
	segs := []CaptureSegment{captureTestSegment(captureTestNow.Add(-2 * time.Minute)), captureTestSegment(captureTestNow.Add(-time.Minute))}
	b := captureTestBook(t, segs)
	o := captureTestOptions()
	p := captureTestRead(t, b, `{"limit":1}`, o)
	if p.Kind != "in_progress" || len(p.Segments) != 0 {
		t.Fatalf("lookahead %+v", p)
	}
	p = captureTestRead(t, b, `{"limit":1,"all":true}`, o)
	if p.Kind != "page" || captureTestAfter(p) != "1.0~1.0" {
		t.Fatalf("all %+v", p)
	}
	for i := range segs {
		segs[i].TimeZone = ""
	}
	b = captureTestBook(t, segs)
	p = captureTestRead(t, b, `{"limit":1}`, o)
	if p.Kind != "page" || captureTestAfter(p) != "1.0" {
		t.Fatalf("no local time %+v", p)
	}
	p = captureTestRead(t, b, `{"after":"1.0","limit":1}`, o)
	if p.Cont || !p.Done || p.Span != "1.1" {
		t.Fatalf("timeless next %+v", p)
	}
}

func TestCaptureLimitsAndBudget(t *testing.T) {
	g := captureTestSegment(captureTestNow.Add(-time.Hour))
	g.Text = strings.Repeat("字", 1900)
	b := captureTestBook(t, []CaptureSegment{g, g, g, g})
	o := captureTestOptions()
	p := captureTestRead(t, b, `{}`, o)
	if len(p.Segments) != 3 || p.Done {
		t.Fatalf("chars %+v", p)
	}
	o.MaxToolResult = 2500
	p = captureTestRead(t, b, `{}`, o)
	if len(p.Segments) != 1 || capturePageTokens(p) > o.MaxToolResult {
		t.Fatalf("budget: %d segments", len(p.Segments))
	}
	o.MaxToolResult = 1
	if _, err := NewCall(b, "s", nil, nil, captureTestNow).Captures(o); err == nil || !strings.Contains(err.Error(), "context.max_tool_result=1") {
		t.Fatal(err)
	}
	empty := NewBook()
	if err := empty.BindDay("2026-10-27", "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCall(empty, "s", nil, nil, captureTestNow).Captures(o); err == nil {
		t.Fatal("empty page escaped budget")
	}
	for _, args := range []string{`{"after":1}`, `{"all":"yes"}`, `{"limit":1.5}`, `[`} {
		if _, err := NewCall(b, "s", json.RawMessage(args), nil, captureTestNow).Captures(o); err == nil || err.Error() != `invalid arguments: after must be a string such as "0", "57.3" or "new"` {
			t.Fatalf("%s: %v", args, err)
		}
	}
}

func TestCaptureProgress(t *testing.T) {
	old := captureTestNow.Add(-time.Hour)
	segs := []CaptureSegment{captureTestSegment(old), captureTestSegment(old.Add(time.Minute)), captureTestSegment(old.Add(20 * time.Minute)), captureTestSegment(old.Add(21 * time.Minute)), captureTestSegment(old.Add(40 * time.Minute))}
	b := captureTestBook(t, segs)
	o := captureTestOptions()
	captureTestRead(t, b, `{"limit":1}`, o)
	submit := func(sid, from, to, extra string) *Call {
		args := fmt.Sprintf(`{"source":{"from":%q,"to":%q}%s}`, from, to, extra)
		c := NewCall(b, sid, json.RawMessage(args), nil, captureTestNow)
		c.FileChanges()
		return c
	}
	if c := submit("s", "#1.0", "#1.4", `,"exchanges":{"merge":[{"into":"missing","from":["x"]}]}`); c.Ack() != nil {
		t.Fatal("rejected ack")
	}
	if b.CaptureProgress("2026-10-27") != "0" {
		t.Fatal("rejected advanced")
	}
	if c := submit("s", "#1.0", "#1.0", ""); c.Ack() == nil {
		t.Fatal("legacy ack lost")
	}
	if b.CaptureProgress("2026-10-27") != "0" {
		t.Fatal("partial span advanced")
	}
	submit("s", "#1.1", "#1.1", "")
	if b.CaptureProgress("2026-10-27") != "0" {
		t.Fatal("partial changesets were stitched")
	}
	submit("unbound", "#1.0", "#1.4", "")
	if b.CaptureProgress("2026-10-27") != "0" {
		t.Fatal("unbound advanced")
	}
	submit("s", "#1.0", "#1.3", "")
	if b.CaptureProgress("2026-10-27") != "1.3" {
		t.Fatal("two spans not advanced")
	}
	p := captureTestRead(t, b, `{"after":"new"}`, o)
	if p.Span != "1.4" || p.Cont || len(p.Segments) != 1 {
		t.Fatalf("new %+v", p)
	}
	clone, err := b.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if clone.CaptureProgress("2026-10-27") != "1.3" {
		t.Fatal("progress not persisted")
	}
	if err := b.BindDay("2026-10-27", "replacement"); err != nil {
		t.Fatal(err)
	}
	submit("s", "#1.4", "#1.4", "")
	if b.CaptureProgress("2026-10-27") != "1.3" {
		t.Fatal("old session advanced")
	}
}

func TestCaptureExchangeBusinessDate(t *testing.T) {
	b := captureTestBook(t, []CaptureSegment{captureTestSegment(captureTestNow)})
	args := json.RawMessage(`{"source":{"from":"#1.0","to":"#1.0"},"exchanges":{"create":[{"from":"#1.0","to":"#1.0","title":"交付","summary":"周五交付","projects":[{"match":"none"}]}]}}`)
	cites := map[string]Citation{"#1.0": {Ref: "#1.0", Speaker: "wearer"}}
	for _, sid := range []string{"s", "unbound"} {
		c := NewCall(b, sid, args, cites, captureTestNow)
		if text := c.FileChanges(); c.Ack() == nil {
			t.Fatal(text)
		}
	}
	st := b.State()
	if len(st.Exchanges) != 2 || st.Exchanges[0].Date != "2026-10-27" || st.Exchanges[1].Date != "2026-10-28" {
		t.Fatalf("dates %+v", st.Exchanges)
	}
	if len(st.Days) != 0 {
		t.Fatal("binding changed daily summaries")
	}
}

func TestCaptureLimitCap(t *testing.T) {
	b := NewBook()
	if err := b.BindDay("2026-10-27", "s"); err != nil {
		t.Fatal(err)
	}
	segs := make([]CaptureSegment, 200)
	for i := range segs {
		segs[i] = CaptureSegment{Speaker: "wearer", Text: "字"}
	}
	for i := range 3 {
		if _, err := b.AppendCaptures("2026-10-27", fmt.Sprint(i), segs); err != nil {
			t.Fatal(err)
		}
	}
	o := captureTestOptions()
	o.MaxChars = 100000
	o.MaxToolResult = 0
	p := captureTestRead(t, b, `{"limit":999}`, o)
	if len(p.Segments) != 500 || p.Done {
		t.Fatalf("cap %d", len(p.Segments))
	}
	p = captureTestRead(t, b, `{"limit":0}`, o)
	if len(p.Segments) != 200 {
		t.Fatalf("default %d", len(p.Segments))
	}
	o.Limit = 17
	for _, args := range []string{`{}`, `{"limit":201}`} {
		p = captureTestRead(t, b, args, o)
		if len(p.Segments) != 17 {
			t.Fatalf("platform cap %d", len(p.Segments))
		}
	}
}

func TestCaptureProgressUsesAcceptedTail(t *testing.T) {
	at := captureTestNow.Add(-time.Hour)
	b := captureTestBook(t, []CaptureSegment{captureTestSegment(at)})
	captureTestRead(t, b, `{}`, captureTestOptions())
	if _, err := b.AppendCaptures("2026-10-27", "later", []CaptureSegment{captureTestSegment(at.Add(time.Minute))}); err != nil {
		t.Fatal(err)
	}
	c := NewCall(b, "s", json.RawMessage(`{"source":{"from":"#1.0","to":"#1.0"}}`), nil, captureTestNow)
	c.FileChanges()
	if b.CaptureProgress("2026-10-27") != "0" {
		t.Fatal("stale tail advanced")
	}
	c = NewCall(b, "s", json.RawMessage(`{"source":{"from":"#1.0","to":"#2.0"}}`), nil, captureTestNow)
	c.FileChanges()
	if b.CaptureProgress("2026-10-27") != "2.0" {
		t.Fatal("complete tail not advanced")
	}
	p := captureTestRead(t, b, `{"after":"new"}`, captureTestOptions())
	if p.Kind != "empty" {
		t.Fatal("new should be empty")
	}
}
