package sim

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/faults"

	"yanshi/internal/model"
	"yanshi/internal/service"
)

func TestScheduleCatchupAndClosedQuiesce(t *testing.T) {
	w, err := New(Options{Seed: 7, Workers: 3, Sessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	text := "定时检查"
	for w.blocked(text) {
		text += "。"
	}
	for i, sid := range w.sessions {
		_, err := w.svc.CreateSchedule(ctx, sid, service.CreateScheduleRequest{ScheduleID: "periodic", Input: model.TextBlocks(text), TimeZone: "UTC", RRule: "FREQ=DAILY;BYHOUR=0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23;BYMINUTE=0;COUNT=3", By: "service"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := w.svc.Close(ctx, sid, "service", "test"); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.clock.Advance(4 * time.Hour)
	if err := w.quiesce(); err != nil {
		t.Fatal(err)
	}
	w.CollectStats()
	if w.Stats.SchedulesFired != 1 || w.Stats.SchedulesSkipped != 2 {
		t.Fatalf("stats %+v", w.Stats)
	}
	if err := w.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// 保留原有综合覆盖的随机轨迹；定时故障覆盖在独立选项下仍逐项要求发生。
func TestSchedulesWithFaults(t *testing.T) {
	t.Parallel()
	total := parallelSeeds(t, seedList(), func(t *testing.T, s uint64) Stats {
		return run(t, s, Options{Faults: true, Attested: true, Schedules: true}).Stats
	})
	requireCoverage(t, total, "SchedulesFired", "SchedulesSkipped", "SchedulesCancelled", "SchedulesChildRejected")
}

// 包含旧失败种子 13/20，继续覆盖长 Run 阻塞定时后的追赶，预算仍为 2000 轮。
func TestScheduledLongRunsWithFaults(t *testing.T) {
	t.Parallel()
	list := seedList()
	if *seed == 0 {
		list = list[:max(len(list)/5, 1)]
	}
	parallelSeeds(t, list, func(t *testing.T, s uint64) Stats {
		return run(t, s, Options{Faults: true, LongRuns: true, Schedules: true, Sessions: 2, Ticks: 1500}).Stats
	})
}

// 压缩后的请求不含原始定时输入时，短任务上限仍以日志计数，不能继续扩张待排空的工作。
func TestScheduledModelStopsAfterThreeToolRounds(t *testing.T) {
	w, err := New(Options{Seed: 13, Workers: 3, Sessions: 1, LongRuns: true, Schedules: true})
	if err != nil {
		t.Fatal(err)
	}
	sid := w.sessions[0]
	ctx := faults.WithSession(context.Background(), sid)
	due := w.clock.Now().Add(time.Second)
	item, err := w.svc.CreateSchedule(ctx, sid, service.CreateScheduleRequest{Input: model.TextBlocks("定时检查"), TimeZone: "UTC", At: due.Format("2006-01-02T15:04:05"), By: "service"})
	if err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(time.Second)
	st, err := w.store.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.FireSchedule(ctx, st, item.Created.GetScheduleId()); err != nil {
		t.Fatal(err)
	}
	runID := st.Active().ID
	if err := w.store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: runID, Attempt: 1, WorkerId: "test"}}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := w.store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: runID, Attempt: 1, Content: model.TextBlocks("已处理")}}}); err != nil {
			t.Fatal(err)
		}
	}
	req := &model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks("压缩摘要")}}, Tools: []model.ToolSpec{{Name: "memory_search"}}}
	resp, err := (&simModel{w: w}).Generate(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 0 || len(resp.Content) == 0 {
		t.Fatal("scheduled workload grew after its bounded work")
	}
}
