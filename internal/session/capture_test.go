package session

import (
	"fmt"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func wearer(text string) *v1.CaptureSegment {
	return &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: text}
}

func other(key, text string) *v1.CaptureSegment {
	return &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: key, Text: text}
}

func captured(id string, segs ...*v1.CaptureSegment) *v1.Event {
	return &v1.Event{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{CaptureId: id, Segments: segs}}}
}

// 旁听转写只进去重窗口与计数：不开启 Run，不进对话上下文（ADR-0036）；Run 进行中也可以写入。
func TestCaptureAppended(t *testing.T) {
	st, err := Reduce(build(created(), captured("cap1", wearer("明天我把报价发给张三"), other("other-1", "好的")),
		requested("r1"), captured("cap2", &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "other-1", Redacted: true})))
	if err != nil {
		t.Fatal(err)
	}
	if c := st.Captures; c.Count != 2 || c.LastSeq != 4 || !st.Captured("cap1") || !st.Captured("cap2") {
		t.Fatalf("captures = %+v", c)
	}
	if len(st.Runs) != 1 {
		t.Fatalf("runs = %d", len(st.Runs))
	}
	for _, e := range st.History {
		if e.GetCaptureAppended() != nil {
			t.Fatal("capture in history")
		}
	}
	if st.Captured("") || st.Captured("cap3") {
		t.Fatal("unknown capture id reported as captured")
	}
}

// 去重窗口有界：超出 MaxRecentCaptures 后最早的 ID 移出窗口；副本追加不影响原状态。
func TestCaptureWindow(t *testing.T) {
	events := []*v1.Event{created()}
	for i := range MaxRecentCaptures + 1 {
		events = append(events, captured(fmt.Sprintf("cap%d", i), wearer("x")))
	}
	st, err := Reduce(build(events...))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(st.Captures.IDs); n != MaxRecentCaptures || st.Captured("cap0") || !st.Captured("cap1") {
		t.Fatalf("window = %d, cap0 %v", n, st.Captured("cap0"))
	}
	if st.Captures.Count != MaxRecentCaptures+1 {
		t.Fatalf("count = %d", st.Captures.Count)
	}
	next := captured("cap-next", wearer("x"))
	next.SessionId, next.Seq = st.SessionID, st.Seq+1
	cp := st.Clone()
	if err := cp.Apply(next); err != nil {
		t.Fatal(err)
	}
	if st.Captured("cap-next") || !cp.Captured("cap-next") {
		t.Fatal("clone shares the capture window")
	}
}

func TestCaptureInvalid(t *testing.T) {
	segs := make([]*v1.CaptureSegment, MaxCaptureSegments+1)
	for i := range segs {
		segs[i] = wearer("x")
	}
	cases := map[string]struct {
		events []*v1.Event
		want   string
	}{
		"no id":         {[]*v1.Event{created(), captured("", wearer("x"))}, "without an id"},
		"empty":         {[]*v1.Event{created(), captured("c")}, "0 segments"},
		"too many":      {[]*v1.Event{created(), captured("c", segs...)}, "201 segments"},
		"no speaker":    {[]*v1.Event{created(), captured("c", &v1.CaptureSegment{Text: "x"})}, "speaker"},
		"wearer key":    {[]*v1.Event{created(), captured("c", &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, SpeakerKey: "w", Text: "x"})}, "wearer with a speaker key"},
		"other no key":  {[]*v1.Event{created(), captured("c", other("", "x"))}, "without a key"},
		"empty text":    {[]*v1.Event{created(), captured("c", wearer(""))}, "redacted false"},
		"redacted text": {[]*v1.Event{created(), captured("c", &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "x", Redacted: true})}, "redacted true"},
		"too long":      {[]*v1.Event{created(), captured("c", wearer(strings.Repeat("话", MaxCaptureText+1)))}, "characters"},
		"duplicate":     {[]*v1.Event{created(), captured("c", wearer("x")), captured("c", wearer("y"))}, "duplicate capture id"},
		"after close":   {[]*v1.Event{created(), closed(), captured("c", wearer("x"))}, "closed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Reduce(build(c.events...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

// 错误中不含转写内容：它会进日志与返回给客户端（ADR-0015）。
func TestCaptureErrorHasNoText(t *testing.T) {
	_, err := Reduce(build(created(), captured("c", wearer(strings.Repeat("秘", MaxCaptureText+1)))))
	if err == nil || strings.Contains(err.Error(), "秘") {
		t.Fatalf("err = %v", err)
	}
}
