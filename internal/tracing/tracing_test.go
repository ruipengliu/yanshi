package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestTraceID(t *testing.T) {
	const want = "4bcf03134590d1e622cc7ff77fbd4398"
	if got := TraceID("run_example"); got != want {
		t.Errorf("TraceID = %q, want %q", got, want)
	}
	if TraceID("run_example") == TraceID("run_other") || TraceID("") != "" {
		t.Fatal("TraceID did not distinguish Run IDs from one another and from an absent ID")
	}
	if got := nonzeroHex(make([]byte, 16)); got != strings.Repeat("0", 31)+"1" {
		t.Fatalf("zero ID was mapped to %q", got)
	}
}

func TestJSONSpan(t *testing.T) {
	var out bytes.Buffer
	tracer := New(slog.New(slog.NewJSONHandler(&out, nil)))
	parent := tracer.Start(RunAttempt, Session("s"), Attempt(2), Worker("w"))
	parent.Set(Run("r"))
	span := parent.Child(ModelCall)
	span.FirstToken()
	span.Set(Call("c"), Count(Retries, 1), LinkRun("r2"))
	span.End(OK)
	span.End(Error)
	parent.End(Done)

	dec := json.NewDecoder(&out)
	var child, attempt map[string]any
	if err := dec.Decode(&child); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&attempt); err != nil {
		t.Fatal(err)
	}
	if dec.More() {
		t.Fatal("unexpected extra span")
	}
	want := map[string]any{
		"msg": "span", "level": "INFO", FieldTraceID: TraceID("r"), FieldParentSpanID: attempt[FieldSpanID],
		FieldName: "model.call", FieldOutcome: "ok", FieldSession: "s", FieldRun: "r", FieldAttempt: float64(2),
		FieldCall: "c", FieldWorker: "w", FieldRetries: float64(1), FieldLinkTraceID: TraceID("r2"),
	}
	spanID := regexp.MustCompile(`^[0-9a-f]{16}$`)
	for _, id := range []any{child[FieldSpanID], attempt[FieldSpanID]} {
		if id, ok := id.(string); !ok || !spanID.MatchString(id) || id == strings.Repeat("0", 16) {
			t.Fatalf("invalid span ID %v", id)
		}
	}
	if child[FieldSpanID] == attempt[FieldSpanID] {
		t.Fatal("child shares its parent's span ID")
	}
	if got := attempt[FieldParentSpanID]; got != "" {
		t.Fatalf("root has parent %v", got)
	}
	for _, key := range []string{"time", FieldStart} {
		if s, ok := child[key].(string); !ok {
			t.Errorf("%s has type %T", key, child[key])
		} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	for _, key := range []string{FieldDuration, FieldFirstToken} {
		if v, ok := child[key].(float64); !ok || v < 0 {
			t.Errorf("%s = %v, expected a nonnegative number", key, child[key])
		}
	}
	for key, v := range want {
		if got := child[key]; !reflect.DeepEqual(got, v) {
			t.Errorf("%s = %v (%T), want %v (%T)", key, got, got, v, v)
		}
	}
	if len(child) != len(want)+5 {
		t.Errorf("unexpected fields in %v", child)
	}
}

func TestDisabledTracer(t *testing.T) {
	ctx := context.Background()
	for _, tr := range []*Tracer{nil, {}, New(nil)} {
		span := tr.Start(ModelCall, Run("r"))
		if tr.Enabled() || span != nil || span.Child(ModelAdmission) != nil || span.Context(ctx) != ctx {
			t.Fatal("disabled tracing returned an active span or changed the context")
		}
		span.Set(Count(Retries, 1))
		span.FirstToken()
		span.End(OK)
		tr.Record(RunEnd, time.Time{}, time.Time{}, Done, Run("r"))
	}
}

func BenchmarkDisabledTracer(b *testing.B) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		tracer *Tracer
	}{{"nil", nil}, {"zero", &Tracer{}}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				span := tc.tracer.Start(RunAttempt, Session("s"), Run("r"), Attempt(1), Worker("w"))
				child := span.Child(ModelCall)
				_ = child.Context(ctx)
				child.FirstToken()
				child.End(OK)
				span.End(Done)
			}
		})
	}
}
