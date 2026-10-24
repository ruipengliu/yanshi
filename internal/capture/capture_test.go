package capture_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/model"
)

// appendLog 写入 SessionCreated、n 批旁听（每批 per 段），每批之后夹一条输入（旁听之间夹着 Run 的事件）。
func appendLog(t *testing.T, n, per int) *memlog.Log {
	t.Helper()
	ctx := context.Background()
	log := memlog.New()
	events := []*v1.Event{{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}}}
	for i := range n {
		segs := make([]*v1.CaptureSegment, per)
		for j := range segs {
			segs[j] = &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: fmt.Sprintf("第 %d 批第 %d 段", i, j)}
		}
		events = append(events,
			&v1.Event{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{CaptureId: fmt.Sprint(i), Segments: segs}}},
			&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: fmt.Sprint("r", i), Input: model.TextBlocks("x")}}})
	}
	for i, e := range events {
		e.SessionId, e.Seq = "s", uint64(i+1)
	}
	if _, err := log.Append(ctx, "s", 0, events...); err != nil {
		t.Fatal(err)
	}
	return log
}

func segments(blocks []*v1.ContentBlock) int {
	n := 0
	for _, b := range blocks {
		if b.GetCapture() != nil {
			n++
		}
	}
	return n
}

// nextAfter 取出页尾给出的下一页位置。
func nextAfter(t *testing.T, out []*v1.ContentBlock) string {
	t.Helper()
	tail := out[len(out)-1].GetText().GetText()
	_, rest, ok := strings.Cut(tail, `after="`)
	if !ok {
		t.Fatalf("no cursor in %q", tail)
	}
	return rest[:strings.Index(rest, `"`)]
}

// 没有时间的旁听从头一页页读完：每页不超过段数上限，下一页从上一页给出的位置接着读，不重不漏；一批可以分在两页。
func TestPagesCoverEveryCaptureOnce(t *testing.T) {
	ctx := context.Background()
	log := appendLog(t, 250, 3) // 跨过多次日志读取（readBatch）
	cp := capture.New(log)
	after, total := "0", 0
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("paging does not terminate")
		}
		out, err := cp.Invoke(ctx, capability.Invocation{SessionID: "s", Arguments: fmt.Sprintf(`{"after":%q,"limit":100}`, after)})
		if err != nil {
			t.Fatal(err)
		}
		n := segments(out)
		if n == 0 || n > 100 {
			t.Fatalf("page %d has %d segments", page, n)
		}
		total += n
		if strings.Contains(model.Text(out), "已读完") {
			break
		}
		after = nextAfter(t, out)
	}
	if total != 750 {
		t.Fatalf("read %d segments, want 750", total)
	}
	if _, err := cp.Invoke(ctx, capability.Invocation{SessionID: "s", Arguments: `{"after":"x"}`}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

// 字符数上限同样分页；只有第一段就超出上限时也读入它（否则永远读不过去）。
func TestPageCharacterLimit(t *testing.T) {
	ctx := context.Background()
	log := appendLog(t, 3, 2)
	out, err := capture.Page(ctx, log, "s", capture.Cursor{}, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := segments(out); n != 1 || nextAfter(t, out) != "2.0" {
		t.Fatalf("page = %d segments, %q", n, model.Text(out))
	}
	empty, err := capture.Page(ctx, log, "none", capture.Cursor{}, 100, capture.MaxChars)
	if err != nil || segments(empty) != 0 || !strings.Contains(model.Text(empty), "已读完") {
		t.Fatalf("empty session = %v, %v", empty, err)
	}
}

// timedLog 写入 SessionCreated 与若干批带时间的旁听（Asia/Shanghai），批之间夹一条输入。
func timedLog(t *testing.T, batches ...[]string) *memlog.Log {
	t.Helper()
	zone, _ := time.LoadLocation("Asia/Shanghai")
	events := []*v1.Event{{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}}}
	for i, b := range batches {
		var segs []*v1.CaptureSegment
		for _, line := range b {
			at, text, _ := strings.Cut(line, " ")
			seg := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: text}
			if at != "-" {
				ts, err := time.ParseInLocation("2006-01-02T15:04", at, zone)
				if err != nil {
					t.Fatal(err)
				}
				seg.SpokenAt, seg.TimeZone = timestamppb.New(ts), "Asia/Shanghai"
			}
			segs = append(segs, seg)
		}
		events = append(events,
			&v1.Event{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{CaptureId: fmt.Sprint(i), Segments: segs}}},
			&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: fmt.Sprint("r", i), Input: model.TextBlocks("x")}}})
	}
	for i, e := range events {
		e.SessionId, e.Seq = "s", uint64(i+1)
	}
	log := memlog.New()
	if _, err := log.Append(context.Background(), "s", 0, events...); err != nil {
		t.Fatal(err)
	}
	return log
}

// 相隔超过交谈间隔处分开交谈，边界可以落在一批中间；到了段数上限而交谈未完时，下一页接着读同一段交谈（续）。
func TestPageByExchange(t *testing.T) {
	ctx := context.Background()
	log := timedLog(t,
		[]string{"2026-10-23T09:00 甲", "2026-10-23T09:02 乙", "2026-10-23T09:30 丙"}, // 事件 2：丙开始第二段交谈
		[]string{"2026-10-23T09:31 丁"}, // 事件 4
		[]string{"2026-10-23T09:33 戊"}, // 事件 6
	)
	read := func(after string) ([]*v1.ContentBlock, string) {
		t.Helper()
		out, err := capture.New(log).Invoke(ctx, capability.Invocation{SessionID: "s", Arguments: fmt.Sprintf(`{"after":%q,"limit":2}`, after)})
		if err != nil {
			t.Fatal(err)
		}
		return out, model.Text(model.ExpandCaptures(out))
	}
	header := "—— 2026-10-23 星期五（当地时间）——\n" +
		"（之后七天：10月24日星期六、10月25日星期日、10月26日星期一、10月27日星期二、10月28日星期三、10月29日星期四、10月30日星期五）\n"
	out, got := read("0")
	if want := "【交谈 x2.0】\n" + header + "#2.0 09:00 [佩戴者] 甲\n#2.1 09:02 [佩戴者] 乙\n" +
		`（交谈 x2.0 到此结束，本页 2 段；下一段交谈用 after="2.1"）`; got != want {
		t.Fatalf("page 1 =\n%s\nwant\n%s", got, want)
	}
	out, got = read(nextAfter(t, out))
	if want := "【交谈 x2.2】\n" + header + "#2.2 09:30 [佩戴者] 丙\n#4.0 09:31 [佩戴者] 丁\n" +
		`（交谈 x2.2 未完，本页 2 段；下一页用 after="4.0~2.2"）`; got != want {
		t.Fatalf("page 2 =\n%s\nwant\n%s", got, want)
	}
	_, got = read(nextAfter(t, out))
	if want := "【交谈 x2.2（续）】\n" + header + "#6.0 09:33 [佩戴者] 戊\n（交谈 x2.2 到此结束，本页 1 段；已读完全部旁听）"; got != want {
		t.Fatalf("page 3 =\n%s\nwant\n%s", got, want)
	}
}

// 有时区的片段显示当地时刻，当地日期变化处注明日期与星期；没有时区的片段不显示时间，也不切分交谈。
func TestPageShowsLocalTime(t *testing.T) {
	ctx := context.Background()
	log := timedLog(t, []string{"2026-10-23T23:58 晚", "2026-10-24T00:01 过了午夜", "- 不知道时间"})
	out, err := capture.Page(ctx, log, "s", capture.Cursor{}, 100, capture.MaxChars)
	if err != nil {
		t.Fatal(err)
	}
	want := "【交谈 x2.0】\n—— 2026-10-23 星期五（当地时间）——\n" +
		"（之后七天：10月24日星期六、10月25日星期日、10月26日星期一、10月27日星期二、10月28日星期三、10月29日星期四、10月30日星期五）\n" +
		"#2.0 23:58 [佩戴者] 晚\n" +
		"—— 2026-10-24 星期六（当地时间）——\n" +
		"（之后七天：10月25日星期日、10月26日星期一、10月27日星期二、10月28日星期三、10月29日星期四、10月30日星期五、10月31日星期六）\n" +
		"#2.1 00:01 [佩戴者] 过了午夜\n#2.2 [佩戴者] 不知道时间\n（交谈 x2.0 到此结束，本页 3 段；已读完全部旁听）"
	if got := model.Text(model.ExpandCaptures(out)); got != want {
		t.Fatalf("page =\n%s\nwant\n%s", got, want)
	}
}
