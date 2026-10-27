package capture_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

func TestBudgetPreservesCharacterBoundary(t *testing.T) {
	text := strings.Repeat("中a", 1000)
	log := timedLog(t, []string{"2026-10-23T23:58 " + text, "2026-10-24T00:01 " + text, "2026-10-24T00:02 " + text})
	opts := capture.Options{Limit: 200, MaxChars: capture.MaxChars, Gap: gap}
	old, cursor, err := capture.Page(context.Background(), log, "s", capture.Cursor{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	// 固定旧实现的 MaxChars 边界与逐字节文本，包含跨日期头与续页位置。
	if segments(old) != 2 || cursor.String() != "2.1~2.0" {
		t.Fatalf("old boundary: %v", cursor)
	}
	// 以下为旧实现此页的完整渲染，防止无预算路径和有预算路径一起改变时漏报。
	want := "【时段 #2.0 起】\n" +
		"—— 2026-10-23 星期五（当地时间）——\n（之后七天：10月24日星期六、10月25日星期日、10月26日星期一、10月27日星期二、10月28日星期三、10月29日星期四、10月30日星期五）\n" +
		"#2.0 23:58 [佩戴者] " + text + "\n" +
		"—— 2026-10-24 星期六（当地时间）——\n（之后七天：10月25日星期日、10月26日星期一、10月27日星期二、10月28日星期三、10月29日星期四、10月30日星期五、10月31日星期六）\n" +
		"#2.1 00:01 [佩戴者] " + text + "\n" +
		`（时段 #2.0 起未完，本页 2 段；下一页用 after="2.1~2.0"）`
	if model.Text(model.ExpandCaptures(old)) != want {
		t.Fatal("legacy rendering changed")
	}
	opts.MaxToolResult = 8000
	got, next, err := capture.Page(context.Background(), log, "s", capture.Cursor{}, opts)
	if err != nil || next != cursor || len(got) != len(old) {
		t.Fatalf("page changed: %v, %v", next, err)
	}
	for i := range old {
		if !proto.Equal(old[i], got[i]) {
			t.Fatalf("block %d changed", i)
		}
	}
}

func TestBudgetReadAllOnce(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		at := time.Date(2026, 10, 23, 23, 50, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		lines = append(lines, at.Format("2006-01-02T15:04")+" "+fmt.Sprintf("段%02d ", i)+strings.Repeat("中English😀", 20))
	}
	log := timedLog(t, lines)
	tool := capture.New(log, nil)
	state := &session.State{}
	state.Captures.Read = map[string]*v1.CaptureRead{}
	seen := map[string]int{}
	pages := 0
	for ; pages < 100; pages++ {
		result, err := tool.InvokeResult(context.Background(), capability.Invocation{
			SessionID: "s", Arguments: `{"after":"new","all":true}`, State: state,
			Agent: &agentdef.Def{Context: agentdef.Context{MaxToolResult: 600}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// 与 runtime.truncateBlocks 的入口判断相同：任何一页都不会触发截断。
		if model.EstimateTokens(result.Content) > 600 {
			t.Fatal("runtime would truncate page")
		}
		for _, b := range result.Content {
			if g := b.GetCapture(); g != nil {
				seen[g.Text]++
			}
		}
		if result.Read == nil {
			break
		}
		// 模拟 Run 完成后提交的位置；下一次只用 after:new 接着读。
		state.Captures.Read[capture.Stream] = result.Read
	}
	if pages < 2 || pages == 100 || len(seen) != len(lines) {
		t.Fatalf("pages=%d, segments=%d", pages, len(seen))
	}
	for text, n := range seen {
		if n != 1 {
			t.Fatalf("%q seen %d times", text, n)
		}
	}
}

func TestBudgetBoundaryAndError(t *testing.T) {
	for _, timed := range []bool{false, true} {
		t.Run(fmt.Sprint(timed), func(t *testing.T) {
			prefix := "- "
			if timed {
				prefix = "2026-10-23T23:58 "
			}
			// 补到估算的字节边界，再增加一个 ASCII 字节，应多占一个 token。
			text := "中文English😀"
			seg := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: text}
			if timed {
				seg.SpokenAt = timestamppb.New(time.Date(2026, 10, 23, 23, 58, 0, 0, time.UTC))
				seg.TimeZone = "UTC"
			}
			for len(model.CaptureText(seg))%3 != 0 {
				seg.Text += "a"
			}
			read := func(text string, budget int) (capability.Result, error) {
				log := timedLog(t, []string{prefix + text, prefix + strings.Repeat("大", 500)})
				return capture.New(log, nil).InvokeResult(context.Background(), capability.Invocation{SessionID: "s", Arguments: `{"all":true}`, Agent: &agentdef.Def{Context: agentdef.Context{MaxToolResult: budget}}})
			}
			budget := 1
			for ; budget < 1000; budget++ {
				if _, err := read(seg.Text, budget); err == nil {
					break
				}
			}
			result, err := read(seg.Text, budget)
			if err != nil || segments(result.Content) != 1 || result.Read == nil {
				t.Fatalf("exact budget: %v", err)
			}
			for _, tc := range []struct {
				text   string
				budget int
			}{{seg.Text, budget - 1}, {seg.Text + "a", budget}} {
				result, err := read(tc.text, tc.budget)
				if err == nil || !strings.Contains(err.Error(), "context.max_tool_result") || result.Read != nil || len(result.Content) != 0 {
					t.Fatalf("too small: %+v, %v", result, err)
				}
			}
		})
	}
}

func TestMinimumBudgetFitsLongestCapture(t *testing.T) {
	// 最大事件序号、批内索引、说话人编号与四字节原话；续页标题及跨年日期头。
	ref := capture.Ref{Seq: ^uint64(0) - 1, Index: session.MaxCaptureSegments - 1}
	g := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: strings.Repeat("x", 32), Text: strings.Repeat("😀", session.MaxCaptureText), SpokenAt: timestamppb.New(time.Date(2026, 12, 28, 23, 59, 0, 0, time.UTC)), TimeZone: "UTC"}
	log := timedLog(t, []string{"- unused"})
	// 用真实日志读取接口的替身交出大序号，无需构造此前全部事件。
	wrapped := &budgetLog{Log: log, events: []*v1.Event{{Seq: ref.Seq, Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{Stream: capture.Stream, Segments: make([]*v1.CaptureSegment, session.MaxCaptureSegments)}}}}}
	for i := range wrapped.events[0].GetCaptureAppended().Segments {
		wrapped.events[0].GetCaptureAppended().Segments[i] = g
	}
	opts := capture.Options{Limit: 1, MaxChars: capture.MaxChars, MaxToolResult: session.MinCaptureToolResult(), Gap: gap, All: true}
	out, _, err := capture.Page(context.Background(), wrapped, "s", capture.Cursor{Last: capture.Ref{Seq: ref.Seq, Index: ref.Index - 1}, Span: capture.Ref{Seq: ref.Seq - 1, Index: 199}}, opts)
	if err != nil || segments(out) != 1 || model.EstimateTokens(out) > opts.MaxToolResult {
		t.Fatalf("longest capture: %v", err)
	}
}

type budgetLog struct {
	eventlog.Log
	events []*v1.Event
}

func (l *budgetLog) Read(_ context.Context, _ string, after uint64, _ int) ([]*v1.Event, error) {
	if after >= l.events[0].GetSeq() {
		return nil, nil
	}
	return l.events, nil
}

func TestBudgetTailForms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		limit int
		tail  string
	}{
		{"done", []string{"2026-10-23T09:00 中English"}, 200, "已读完目前的全部旁听"},
		{"ended", []string{"2026-10-23T09:00 中English", "2026-10-23T10:00 另一时段"}, 200, `下一时段用 after="2.0"`},
		{"continued", []string{"2026-10-23T09:00 中English", "2026-10-23T09:01 同一时段"}, 1, `下一页用 after="2.0~2.0"`},
		{"untimed", []string{"- 中English", "- 没有时间"}, 1, `下一时段用 after="2.0"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := timedLog(t, tc.lines)
			opts := capture.Options{Limit: tc.limit, MaxChars: capture.MaxChars, MaxToolResult: 300, Gap: gap, All: true}
			out, next, err := capture.Page(context.Background(), log, "s", capture.Cursor{}, opts)
			if err != nil || segments(out) != 1 || !strings.Contains(model.Text(out), tc.tail) || model.EstimateTokens(out) > opts.MaxToolResult {
				t.Fatalf("page: %v, %v", out, err)
			}
			if tc.name == "continued" {
				out, _, err = capture.Page(context.Background(), log, "s", next, opts)
				if err != nil || segments(out) != 1 || !strings.Contains(model.Text(out), "起（续）") || model.EstimateTokens(out) > opts.MaxToolResult {
					t.Fatalf("continuation: %v, %v", out, err)
				}
			}
		})
	}
}
