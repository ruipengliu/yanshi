package service_test

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

// TestIndexTouchedAtMostEveryMinute：连续对话中，与上一个 Run 在同一个 TouchEvery 时段内的输入不更新索引的
// 最后输入时间（省掉每个 Run 一次写入），但索引的滞后始终不到 TouchEvery，间隔很短的长对话也是如此。
func TestIndexTouchedAtMostEveryMinute(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clk}
	index := lifecycle.NewMemIndex()
	svc := &service.Service{Store: store, Queue: memqueue.New(clk), Agents: agents, Index: index}
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	lastInput := func() time.Time {
		t.Helper()
		x, err := index.Get(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		return x.LastInputAt
	}
	// finish 让当前 Run 结束，下一次输入开启新 Run。
	finish := func() {
		t.Helper()
		st, _ := svc.Load(ctx, sid)
		a := st.Active()
		if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_RunInterrupted{RunInterrupted: &v1.RunInterrupted{RunId: a.ID, By: "test"}}}); err != nil {
			t.Fatal(err)
		}
	}
	submit := func() {
		t.Helper()
		if _, err := svc.Submit(ctx, sid, model.TextBlocks("hi")); err != nil {
			t.Fatal(err)
		}
		finish()
	}
	clk.Advance(time.Hour) // 09:00 整点开始的时段
	submit()
	first := clk.Now()
	if !lastInput().Equal(first) {
		t.Fatalf("first input not recorded: %v", lastInput())
	}
	clk.Advance(lifecycle.TouchEvery / 4)
	submit()
	if !lastInput().Equal(first) {
		t.Fatalf("index touched again within the same period: %v", lastInput())
	}
	// 每隔 50 秒一次输入，持续十分钟：每次比较时索引与最近的输入相差都不到 TouchEvery。
	writes := 0
	for range 12 {
		before := lastInput()
		clk.Advance(50 * time.Second)
		submit()
		if !lastInput().Equal(before) {
			writes++
		}
		if lag := clk.Now().Sub(lastInput()); lag >= lifecycle.TouchEvery {
			t.Fatalf("index lags the latest input by %s", lag)
		}
	}
	if writes == 12 {
		t.Fatal("every input touched the index")
	}
}
