package runtime_test

import (
	"strings"
	"testing"
	"time"

	"yanshi/internal/model"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/workqueue/memqueue"
)

func TestWorkerScheduleParkFireAndTranscript(t *testing.T) {
	b := newBgWorld(t, nil)
	due := b.clk.Now().Add(time.Hour)
	_, err := b.core.Service.CreateSchedule(b.ctx, b.sid, service.CreateScheduleRequest{ScheduleID: "one", Input: model.TextBlocks("提醒"), At: due.Format("2006-01-02T15:04:05"), TimeZone: "UTC", By: "service"})
	if err != nil {
		t.Fatal(err)
	}
	b.drain()
	if !b.core.Stores.Queue.(*memqueue.Queue).Has(b.sid) {
		t.Fatal("future schedule left queue")
	}
	b.clk.Advance(time.Hour - time.Second)
	b.drain()
	if b.state().Active() != nil {
		t.Fatal("fired early")
	}
	b.clk.Advance(time.Second)
	if _, err := b.w.Step(b.ctx); err != nil {
		t.Fatal(err)
	}
	st := b.state()
	if st.Active() == nil {
		t.Fatal("due schedule did not request Run")
	}
	messages := runtime.Transcript(st, 0)
	text := model.Text(messages[len(messages)-1].Content)
	if !strings.Contains(text, "【定时 one 到点：") || !strings.HasSuffix(text, "提醒") {
		t.Fatalf("transcript %q", text)
	}
	before := text
	b.clk.Advance(366 * 24 * time.Hour)
	messages = runtime.Transcript(st, 0)
	if model.Text(messages[len(messages)-1].Content) != before {
		t.Fatal("transcript prefix changed with current time")
	}
}

func TestWorkerScheduleWaitsForActiveRun(t *testing.T) {
	b := newBgWorld(t, nil)
	due := b.clk.Now().Add(time.Second)
	_, err := b.core.Service.CreateSchedule(b.ctx, b.sid, service.CreateScheduleRequest{ScheduleID: "one", Input: model.TextBlocks("提醒"), At: due.Format("2006-01-02T15:04:05"), TimeZone: "UTC", By: "service"})
	if err != nil {
		t.Fatal(err)
	}
	b.submit("先处理")
	first := b.state().Active().ID
	b.clk.Advance(time.Second)
	if _, err := b.w.Step(b.ctx); err != nil {
		t.Fatal(err)
	}
	if st := b.state(); st.Active() == nil || st.Active().ID != first || st.Schedules["one"].NextDue.IsZero() {
		t.Fatal("scheduled Run overtook queued input")
	}
	b.drain()
	if !b.state().Schedules["one"].NextDue.IsZero() {
		t.Fatal("schedule did not fire after Run ended")
	}
}
