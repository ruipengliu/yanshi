package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
)

// 先记录真实来源的调用结果，再预置待执行的 schedule，避免模型替身忽略前一条结果。
func TestScheduleApprovalGate(t *testing.T) {
	for _, source := range []string{"clean", "phone__read_file", "mcp__read", "scheduled"} {
		for _, action := range []string{"create", "cancel", "list", "invalid", "bad rule"} {
			for _, approve := range []bool{false, true} {
				name := source + "/" + action + "/deny"
				if approve {
					name = source + "/" + action + "/approve"
				}
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					clk := clock.NewFake(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
					log, q := memlog.New(), memqueue.New(clk)
					store := &session.Store{Log: log, Waking: log.Waking(q), IDs: ids.Sequential("id"), Clock: clk}
					agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "v", Version: "1", Model: "v/any", Capabilities: []string{"schedule"}})
					if err != nil {
						t.Fatal(err)
					}
					svc := &service.Service{Store: store, Agents: agents}
					sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "v", TimeZone: "Asia/Shanghai"})
					if err != nil {
						t.Fatal(err)
					}
					if action == "cancel" {
						_, err := svc.CreateSchedule(ctx, sid, service.CreateScheduleRequest{ScheduleID: "target", Input: model.TextBlocks("周报"), At: "2026-10-30T17:00", By: "end_user"})
						if err != nil {
							t.Fatal(err)
						}
					}
					args := `{"action":"create","input":"周报","at":"2026-10-30T17:00"}`
					switch action {
					case "cancel":
						args = `{"action":"cancel","schedule_id":"target"}`
					case "list":
						args = `{"action":"list"}`
					case "invalid":
						args = `{"action":"create","input":5}`
					case "bad rule":
						args = `{"action":"create","input":"周报","rrule":"FREQ=DAILY"}`
					}
					st, err := store.Load(ctx, sid)
					if err != nil {
						t.Fatal(err)
					}
					runID := "r1"
					b := &logBuilder{}
					if source == "scheduled" {
						_, err := svc.CreateSchedule(ctx, sid, service.CreateScheduleRequest{ScheduleID: "trigger", Input: model.TextBlocks("安排周报"), At: clk.Now().Add(time.Second).Format("2006-01-02T15:04:05"), TimeZone: "UTC", By: "service"})
						if err != nil {
							t.Fatal(err)
						}
						clk.Advance(time.Second)
						st, err = store.Load(ctx, sid)
						if err != nil {
							t.Fatal(err)
						}
						if err := svc.FireSchedule(ctx, st, "trigger"); err != nil {
							t.Fatal(err)
						}
						runID = st.Active().ID
					} else {
						b.add(&v1.RunRequested{RunId: runID, Input: model.TextBlocks("请安排周报")})
					}
					b.add(&v1.AttemptStarted{RunId: runID, Attempt: 1})
					if source != "clean" && source != "scheduled" {
						b.add(&v1.AssistantMessage{RunId: runID, Attempt: 1, ToolCalls: []*v1.ToolCall{{CallId: "read", Capability: source, ArgumentsJson: `{}`}}}).add(&v1.ToolResult{RunId: runID, Attempt: 1, CallId: "read", Content: model.TextBlocks("周报资料")})
					}
					b.add(&v1.AssistantMessage{RunId: runID, Attempt: 1, ToolCalls: []*v1.ToolCall{{CallId: "schedule-call", Capability: "schedule", ArgumentsJson: args}}})
					if err := store.Commit(ctx, st, b.events...); err != nil {
						t.Fatal(err)
					}
					gw := model.NewGateway()
					gw.Register("v", callOnce{})
					w := New(Config{ID: "w", Store: store, Queue: q, Agents: agents, Model: gw, Catalog: &capability.Catalog{Local: capability.NewRegistry(svc.ScheduleCapability())}})
					if err := q.Enqueue(ctx, sid, workqueue.Class{}); err != nil {
						t.Fatal(err)
					}
					step := func() {
						t.Helper()
						for range 15 {
							if _, err := w.Step(ctx); err != nil {
								t.Fatal(err)
							}
						}
					}
					step()
					events, err := eventlog.ReadAll(ctx, log, sid, 0)
					if err != nil {
						t.Fatal(err)
					}
					gated := source != "clean" && (action == "create" || action == "cancel")
					approvals := 0
					for _, e := range events {
						if a := e.GetApprovalRequested(); a != nil {
							approvals++
							if !strings.Contains(a.GetSummary(), "周报") {
								t.Fatalf("summary %q", a.GetSummary())
							}
						}
						if gated && e.GetToolCallStarted().GetCallId() == "schedule-call" {
							t.Fatal("executed before approval")
						}
					}
					if (approvals == 1) != gated || approvals > 1 {
						t.Fatalf("approvals %d, gated %v", approvals, gated)
					}
					if gated {
						if err := svc.Decide(ctx, sid, "schedule-call", approve, "u"); err != nil {
							t.Fatal(err)
						}
						step()
					}
					final, err := store.Load(ctx, sid)
					if err != nil {
						t.Fatal(err)
					}
					if final.Active() != nil {
						t.Fatal("run did not finish")
					}
					want := 0
					if action == "create" && (!gated || approve) || action == "cancel" && gated && !approve {
						want = 1
					}
					if len(final.OpenSchedules()) != want {
						t.Fatalf("active schedules %d, want %d", len(final.OpenSchedules()), want)
					}
					if action == "invalid" || action == "bad rule" {
						events, err := eventlog.ReadAll(ctx, log, sid, 0)
						if err != nil {
							t.Fatal(err)
						}
						found := false
						for _, e := range events {
							if r := e.GetToolResult(); r.GetCallId() == "schedule-call" {
								found = r.GetIsError()
							}
						}
						if !found {
							t.Fatal("invalid arguments did not return error")
						}
					}
				})
			}
		}
	}
}
