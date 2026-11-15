package janitor_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
)

func TestScheduledSessionsSurviveIdle(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	active, stale := e.session(t, "day"), e.session(t, "day")
	due := e.clk.Now().Add(7 * 24 * time.Hour)
	for _, sid := range []string{active, stale} {
		if _, err := e.core.Service.CreateSchedule(ctx, sid, service.CreateScheduleRequest{ScheduleID: "s", Input: model.TextBlocks("提醒"), At: due.Format("2006-01-02T15:04"), TimeZone: "UTC", By: "service"}); err != nil {
			t.Fatal(err)
		}
		row, err := e.stors.Index.Get(ctx, "u", sid)
		if err != nil || !row.NextDue.Equal(due) {
			t.Fatalf("create index %v %v", row, err)
		}
	}
	if err := e.core.Service.CancelSchedule(ctx, stale, "s", "end_user"); err != nil {
		t.Fatal(err)
	}
	row, err := e.stors.Index.Get(ctx, "u", stale)
	if err != nil || !row.NextDue.IsZero() {
		t.Fatalf("cancel index %v %v", row, err)
	}
	// 分别模拟创建和取消的日志已提交、索引未更新。
	if err := e.stors.Index.SetNextDue(ctx, "u", active, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.stors.Index.SetNextDue(ctx, "u", stale, due); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Hour)
	j := e.core.Janitor("j", sandbox.NewFake(), map[string]lifecycle.Retention{"bl": {CloseAfterIdle: time.Hour}}, 0)
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{active, stale} {
		st, err := e.core.Service.Load(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if (st.Closed != nil) != (sid == stale) {
			t.Fatalf("closed %s: %v", sid, st.Closed)
		}
	}
	// 关闭入口自己也要挡住读取投影之后的并发创建，不能只依靠 Janitor 的预检查。
	if err := e.core.Service.Close(ctx, active, "retention", "idle"); err != nil {
		t.Fatal(err)
	}
	st, err := e.core.Service.Load(ctx, active)
	if err != nil || st.Closed != nil {
		t.Fatalf("idle close %v %v", st, err)
	}
	if err := e.core.Service.Close(ctx, active, "u", "manual"); err != nil {
		t.Fatal(err)
	}
	st, err = e.core.Service.Load(ctx, active)
	if err != nil || st.Closed == nil {
		t.Fatalf("manual close %v %v", st, err)
	}
}

func TestScheduleReconciliationPages(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	due := e.clk.Now().Add(7 * 24 * time.Hour)
	// 显式使用有序 ID，使前 100 行始终有效，最后一行才是陈旧的豁免。数据槽固定为 000、EndUser 槽是 "u" 的槽位
	// （0e7），ID 的排序因此只由末尾的序号决定。
	for i := range 101 {
		sid, err := e.core.Service.Create(ctx, service.CreateRequest{SessionID: fmt.Sprintf("ses_v1_000_0e7_page-%03d", i), BusinessLine: "bl", EndUser: "u", Agent: "day"})
		if err != nil {
			t.Fatal(err)
		}
		if i < 100 {
			if _, err := e.core.Service.CreateSchedule(ctx, sid, service.CreateScheduleRequest{ScheduleID: "s", Input: model.TextBlocks("提醒"), At: due.Format("2006-01-02T15:04"), TimeZone: "UTC", By: "service"}); err != nil {
				t.Fatal(err)
			}
		} else if err := e.stors.Index.SetNextDue(ctx, "u", sid, due); err != nil {
			t.Fatal(err)
		}
	}
	e.clk.Advance(2 * time.Hour)
	j := e.core.Janitor("j", sandbox.NewFake(), map[string]lifecycle.Retention{"bl": {CloseAfterIdle: time.Hour}}, 0)
	for range 2 {
		if err := j.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st, err := e.core.Service.Load(ctx, "ses_v1_000_0e7_page-100")
	if err != nil || st.Closed == nil {
		t.Fatalf("last page blocked: %v %v", st, err)
	}
}

func TestScheduledSessionAbsoluteDeletion(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sid, err := e.core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "day", DeleteAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.core.Service.CreateSchedule(ctx, sid, service.CreateScheduleRequest{ScheduleID: "s", Input: model.TextBlocks("提醒"), At: e.clk.Now().Add(7 * 24 * time.Hour).Format("2006-01-02T15:04"), TimeZone: "UTC", By: "service"}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Hour)
	if err := e.core.Janitor("j", sandbox.NewFake(), nil, 0).Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if gone, err := lifecycle.Deleted(ctx, e.stors.Deletions, sid); err != nil || !gone {
		t.Fatalf("deadline ignored: %v %v", gone, err)
	}
}
