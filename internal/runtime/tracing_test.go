package runtime_test

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/model/script"
	"yanshi/internal/node"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/tracing"
	"yanshi/internal/workqueue"
)

func TestRunTracing(t *testing.T) {
	for _, tc := range []string{"complete", "handoff", "interrupt", "close", "retry"} {
		t.Run(tc, func(t *testing.T) {
			ctx := t.Context()
			tracer, recorder := tracing.NewRecorder()
			clk := clock.NewFake(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
			start := clk.Now()
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "test", Version: "1", Model: "script/test"})
			if err != nil {
				t.Fatal(err)
			}
			gateway := model.NewGateway()
			guard := &model.Guard{Name: "script", Provider: script.Provider{}, Limits: model.Limits{MaxConcurrency: 1}}
			if tc == "retry" {
				guard.Provider = &retryTraceModel{}
				guard.Limits.Retries, guard.Limits.Backoff = 1, time.Nanosecond
			}
			gateway.Register("script", guard)
			core := app.NewCore(app.Memory(clk), app.Options{
				Clock: clk, Agents: agents, IDs: ids.Sequential, Model: gateway, Blobs: artifact.NewMemBlobs(),
				NodeAuth: node.InsecureDevAuth{}, Tracer: tracer,
			})
			sid, err := core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "test"})
			if err != nil {
				t.Fatal(err)
			}
			input := model.TextBlocks("private text")
			res, err := core.Service.SubmitWithID(ctx, sid, "input1", input)
			if err != nil {
				t.Fatal(err)
			}
			if duplicate, err := core.Service.SubmitWithID(ctx, sid, "input1", input); err != nil || !duplicate.Duplicate {
				t.Fatalf("duplicate Submit = %v, %v", duplicate, err)
			}

			step := func(w *runtime.Worker) {
				t.Helper()
				if did, err := w.Step(ctx); err != nil || !did {
					t.Fatalf("Step = %v, %v", did, err)
				}
			}
			worker := runtime.New(core.WorkerConfig("w1", workqueue.Pool{}, nil))
			clk.Advance(10 * time.Millisecond)
			step(worker)
			switch tc {
			case "handoff":
				if err := worker.Stop(ctx); err != nil {
					t.Fatal(err)
				}
				worker = runtime.New(core.WorkerConfig("w2", workqueue.Pool{}, nil))
				step(worker)
			case "interrupt":
				if err := core.Service.Interrupt(ctx, sid, res.RunID); err != nil {
					t.Fatal(err)
				}
				if err := core.Service.Interrupt(ctx, sid, res.RunID); err != nil {
					t.Fatal(err)
				}
			case "close":
				if err := core.Service.Close(ctx, sid, "user", "private reason"); err != nil {
					t.Fatal(err)
				}
			}
			step(worker)

			// 追踪记录在结束时输出。子 span 先完成，父子 ID 表示调用关系。
			fields := tracedFields(t, recorder.Records(), sid, res.RunID)
			var got []string
			for _, f := range fields {
				got = append(got, f[tracing.FieldName].String()+"/"+f[tracing.FieldOutcome].String())
			}
			want := []string{"run.submit/ok", "queue.claim/ok"}
			if tc == "handoff" {
				want = append(want, "run.attempt/handoff", "queue.claim/ok")
			}
			if tc == "interrupt" || tc == "close" {
				want = append(want, "run.end/interrupted", "run.attempt/interrupted")
			} else {
				if tc == "retry" {
					want = append(want, "model.admission/ok")
				}
				want = append(want, "model.admission/ok", "model.call/ok", "run.attempt/done", "run.end/done")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("spans = %v, want %v", got, want)
			}
			if fields[1][tracing.FieldQueueWait].Float64() != 10 || fields[1][tracing.FieldWorker].String() != "w1" {
				t.Fatalf("first claim = %v", fields[1])
			}
			if tc == "interrupt" || tc == "close" {
				checkRunEnd(t, fields[len(fields)-2], start)
				return
			}
			admission, call, attempt, end := fields[len(fields)-4], fields[len(fields)-3], fields[len(fields)-2], fields[len(fields)-1]
			checkRunEnd(t, end, start)
			if admission[tracing.FieldParentSpanID].String() != call[tracing.FieldSpanID].String() ||
				call[tracing.FieldParentSpanID].String() != attempt[tracing.FieldSpanID].String() {
				t.Fatalf("model spans have the wrong parents: %v; %v; %v", admission, call, attempt)
			}
			if first := call[tracing.FieldFirstToken]; first.Kind() != slog.KindFloat64 ||
				first.Float64() < 0 || first.Float64() > call[tracing.FieldDuration].Float64() {
				t.Fatalf("invalid first token duration in %v", call)
			}
			var wantRetries int64
			if tc == "retry" {
				wantRetries = 1
				if fields[2][tracing.FieldParentSpanID].String() != call[tracing.FieldSpanID].String() {
					t.Fatalf("first admission has the wrong parent: %v", fields[2])
				}
			}
			if retries := call[tracing.FieldRetries]; retries.Kind() != slog.KindInt64 || retries.Int64() != wantRetries {
				t.Fatalf("invalid retry count in %v", call)
			}
			if tc == "handoff" {
				first := fields[2]
				if first[tracing.FieldAttempt].Uint64() != 1 || attempt[tracing.FieldAttempt].Uint64() != 2 ||
					first[tracing.FieldWorker].String() != "w1" || attempt[tracing.FieldWorker].String() != "w2" {
					t.Fatalf("handoff attempts = %v; %v", first, attempt)
				}
			}
		})
	}
}

type retryTraceModel struct {
	script.Provider
	failed bool
}

func (m *retryTraceModel) Generate(ctx context.Context, req *model.Request, delta func(model.Delta)) (*model.Response, error) {
	if !m.failed {
		m.failed = true
		return nil, io.ErrUnexpectedEOF
	}
	return m.Provider.Generate(ctx, req, delta)
}

func checkRunEnd(t *testing.T, f map[string]slog.Value, start time.Time) {
	t.Helper()
	if f[tracing.FieldStart].String() != start.Format(time.RFC3339Nano) || f[tracing.FieldDuration].Float64() != 10 {
		t.Fatalf("Run duration = %v", f)
	}
}

func tracedFields(t *testing.T, records []slog.Record, sid, runID string) []map[string]slog.Value {
	t.Helper()
	allowed := map[string]bool{
		tracing.FieldTraceID: true, tracing.FieldSpanID: true, tracing.FieldParentSpanID: true, tracing.FieldName: true,
		tracing.FieldStart: true, tracing.FieldDuration: true, tracing.FieldOutcome: true, tracing.FieldSession: true,
		tracing.FieldRun: true, tracing.FieldAttempt: true, tracing.FieldWorker: true, tracing.FieldQueueWait: true,
		tracing.FieldFirstToken: true, tracing.FieldRetries: true,
	}
	var fields []map[string]slog.Value
	for _, rec := range records {
		if rec.Message != "span" {
			t.Fatalf("unexpected log message %q", rec.Message)
		}
		f := make(map[string]slog.Value)
		rec.Attrs(func(a slog.Attr) bool {
			if _, dup := f[a.Key]; dup || !allowed[a.Key] {
				t.Fatalf("unexpected span field %q", a.Key)
			}
			f[a.Key] = a.Value
			return true
		})
		if f[tracing.FieldSession].String() != sid {
			t.Fatalf("unexpected Session in %v", f)
		}
		if f[tracing.FieldName].String() == "queue.claim" {
			_, hasRun := f[tracing.FieldRun]
			_, hasTrace := f[tracing.FieldTraceID]
			if hasRun || hasTrace {
				t.Fatalf("claim has a Run or trace ID: %v", f)
			}
		} else if f[tracing.FieldRun].String() != runID || f[tracing.FieldTraceID].String() != tracing.TraceID(runID) {
			t.Fatalf("unexpected Run or trace ID in %v", f)
		}
		fields = append(fields, f)
	}
	return fields
}
