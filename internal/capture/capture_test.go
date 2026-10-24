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

// 从 after=0 一页页读完：每页整批读入、不超过段数上限，下一页从上一页给出的位置接着读，不重不漏。
func TestPagesCoverEveryCaptureOnce(t *testing.T) {
	ctx := context.Background()
	log := appendLog(t, 250, 3) // 跨过多次日志读取（readBatch）
	cp := capture.New(log)
	after, total := uint64(0), 0
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("paging does not terminate")
		}
		out, err := cp.Invoke(ctx, capability.Invocation{SessionID: "s", Arguments: fmt.Sprintf(`{"after":%d,"limit":100}`, after)})
		if err != nil {
			t.Fatal(err)
		}
		n := segments(out)
		if n > 100 || n%3 != 0 {
			t.Fatalf("page %d has %d segments", page, n)
		}
		total += n
		tail := out[len(out)-1].GetText().GetText()
		if strings.Contains(tail, "已读完") {
			break
		}
		if _, err := fmt.Sscanf(tail[strings.Index(tail, "after=")+len("after="):], "%d", &after); err != nil {
			t.Fatalf("no cursor in %q", tail)
		}
	}
	if total != 750 {
		t.Fatalf("read %d segments, want 750", total)
	}
}

// 字符数上限同样分页；只有第一批就超出上限时也读入它（否则永远读不过去）。
func TestPageCharacterLimit(t *testing.T) {
	ctx := context.Background()
	log := appendLog(t, 3, 2)
	out, err := capture.Page(ctx, log, "s", 0, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := segments(out); n != 2 || !strings.Contains(model.Text(out), "未读完") {
		t.Fatalf("page = %d segments, %q", n, model.Text(out))
	}
	empty, err := capture.Page(ctx, log, "none", 0, 100, capture.MaxChars)
	if err != nil || segments(empty) != 0 || !strings.Contains(model.Text(empty), "已读完") {
		t.Fatalf("empty session = %v, %v", empty, err)
	}
}

// 有时区的片段显示当地时刻，当地日期变化处（含本页第一段）注明日期与星期；没有时区的片段不显示时间。
func TestPageShowsLocalTime(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	at := func(s string) *timestamppb.Timestamp {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return timestamppb.New(ts)
	}
	seg := func(text, spoken, zone string) *v1.CaptureSegment {
		return &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: text, SpokenAt: at(spoken), TimeZone: zone}
	}
	events := []*v1.Event{
		{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}},
		{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{CaptureId: "a", Segments: []*v1.CaptureSegment{
			seg("早", "2026-10-23T01:05:00Z", "Asia/Shanghai"),
			seg("晚", "2026-10-23T15:59:00Z", "Asia/Shanghai"),
			seg("过了午夜", "2026-10-23T16:01:00Z", "Asia/Shanghai"),
			seg("不知道时区", "2026-10-23T16:02:00Z", ""),
		}}}},
	}
	for i, e := range events {
		e.SessionId, e.Seq = "s", uint64(i+1)
	}
	if _, err := log.Append(ctx, "s", 0, events...); err != nil {
		t.Fatal(err)
	}
	out, err := capture.Page(ctx, log, "s", 0, 100, capture.MaxChars)
	if err != nil {
		t.Fatal(err)
	}
	want := "—— 2026-10-23 星期五（当地时间）——\n" +
		"（之后七天：10月24日星期六、10月25日星期日、10月26日星期一、10月27日星期二、10月28日星期三、10月29日星期四、10月30日星期五）\n" +
		"09:05 [佩戴者] 早\n23:59 [佩戴者] 晚\n" +
		"—— 2026-10-24 星期六（当地时间）——\n" +
		"（之后七天：10月25日星期日、10月26日星期一、10月27日星期二、10月28日星期三、10月29日星期四、10月30日星期五、10月31日星期六）\n" +
		"00:01 [佩戴者] 过了午夜\n[佩戴者] 不知道时区\n（已读完：本页 4 段，之后没有更多旁听）"
	if got := model.Text(model.ExpandCaptures(out)); got != want {
		t.Fatalf("page =\n%s\nwant\n%s", got, want)
	}
}
