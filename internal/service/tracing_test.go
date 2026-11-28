package service_test

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/delegation"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
)

func TestBackgroundResultTracing(t *testing.T) {
	for _, name := range []string{"device", "device after projection trim", "delegation"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			dispatched := time.Now().Add(-time.Hour)
			clk := clock.NewFake(dispatched)
			tracer, recorder := tracing.NewRecorder()
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "script/any"})
			if err != nil {
				t.Fatal(err)
			}
			core := app.NewCore(app.Memory(clk), app.Options{Clock: clk, Agents: agents, Tracer: tracer})
			sid, err := core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
			if err != nil {
				t.Fatal(err)
			}
			created, err := core.Service.Submit(ctx, sid, model.TextBlocks("private"))
			if err != nil {
				t.Fatal(err)
			}
			run, call := created.RunID, "call_1"
			nodeID, capability, kind := "device", "phone__note", "node"
			if name == "delegation" {
				nodeID, capability, kind = delegation.NodeID(sid), "agent__helper", "delegation"
			}
			st, err := core.Service.Load(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			deadline := timestamppb.New(dispatched.Add(2 * time.Hour))
			err = core.Store.Commit(ctx, st,
				&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: run, Attempt: 1, WorkerId: "w"}}},
				&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: run, Attempt: 1,
					ToolCalls: []*v1.ToolCall{{CallId: call, Capability: capability}}}}},
				&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: run, Attempt: 1,
					CallId: call, NodeId: nodeID, Deadline: deadline}}},
				&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: run, Attempt: 1, CallId: call,
					Backgrounded: &v1.Backgrounded{NodeId: nodeID, Deadline: deadline}}}},
				&v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: run, Attempt: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			if name == "device after projection trim" {
				for i := range session.KeepRuns + 1 {
					later := fmt.Sprintf("run_later_%d", i)
					if err := core.Store.Commit(ctx, st,
						&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: later}}},
						&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: later, Attempt: 1, WorkerId: "w"}}},
						&v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: later, Attempt: 1}}}); err != nil {
						t.Fatal(err)
					}
				}
				if st.Run(run) != nil {
					t.Fatal("original Run still has full projection")
				}
			}
			content := model.TextBlocks("private result")
			linked := ""
			writeStart := time.Now()
			if kind == "delegation" {
				linked = tracing.TraceID("run_child")
				span := tracer.Start(tracing.ToolResult, tracing.Session(sid), tracing.Run(run), tracing.Call(call),
					tracing.Kind(tracing.Delegation), tracing.LinkRun("run_child"))
				err = core.Service.DelegationResult(span.Context(ctx), sid, run, call, content, false, nil)
			} else {
				if err := core.Hub.Inbox.Put(ctx, "u", nodeID, &v1.Invoke{SessionId: sid, RunId: run, CallId: call}); err != nil {
					t.Fatal(err)
				}
				err = core.Hub.Result(ctx, "u", nodeID, &v1.InvokeResult{CallId: call, Content: content})
			}
			if err != nil {
				t.Fatal(err)
			}
			records := recorder.Records()
			if len(records) != 2 {
				t.Fatalf("expected submit and result spans, got %v", records)
			}
			f := make(map[string]slog.Value)
			records[1].Attrs(func(a slog.Attr) bool { f[a.Key] = a.Value; return true })
			var link string
			if value, ok := f[tracing.FieldLinkTraceID]; ok {
				link = value.String()
			}
			if f[tracing.FieldName].String() != "tool.result" || f[tracing.FieldOutcome].String() != "backgrounded" ||
				f[tracing.FieldTraceID].String() != tracing.TraceID(run) || link != linked ||
				f[tracing.FieldSession].String() != sid || f[tracing.FieldRun].String() != run ||
				f[tracing.FieldCall].String() != call || f[tracing.FieldKind].String() != kind {
				t.Fatalf("background result span = %v", f)
			}
			start, err := time.Parse(time.RFC3339Nano, f[tracing.FieldStart].String())
			if err != nil {
				t.Fatal(err)
			}
			if name == "device" && !start.Equal(dispatched) || name != "device" && start.Before(writeStart) {
				t.Errorf("result span started at %s", start)
			}
		})
	}
}
