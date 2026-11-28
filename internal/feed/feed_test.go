package feed

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
)

// flakyWait 的前 n 次 Wait 立即失败（数据库短暂不可用）。
type flakyWait struct {
	*memlog.Log
	mu sync.Mutex
	n  int
}

func (l *flakyWait) Wait(ctx context.Context, sid string, after uint64) (uint64, error) {
	l.mu.Lock()
	fail := l.n > 0
	l.n--
	l.mu.Unlock()
	if fail {
		return 0, errors.New("database unavailable")
	}
	return l.Log.Wait(ctx, sid, after)
}

type chanSink struct{ events chan *v1.Event }

func (s chanSink) Event(e *v1.Event) error { s.events <- e; return nil }
func (chanSink) Delta(live.Delta) error    { return nil }
func (chanSink) Ping() error               { return nil }
func (chanSink) Flush() error              { return nil }

// 等待日志前进的出错是暂时的：恢复后流继续推送新事件，而不是停在那里只保活。
func TestStreamRecoversFromWaitErrors(t *testing.T) {
	waitRetryMin, waitRetryMax = time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &flakyWait{Log: memlog.New(), n: 3}
	ev := func(i int) *v1.Event {
		return &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r" + string(rune('0'+i))}}}
	}
	if _, err := log.Append(ctx, "ses_v1_000_000_s1", 0, nil, ev(1)); err != nil {
		t.Fatal(err)
	}
	sink := chanSink{events: make(chan *v1.Event, 8)}
	stopped := make(chan struct{})
	go func() {
		_ = Source{Log: log, Live: live.Discard{}}.Stream(ctx, &session.State{SessionID: "ses_v1_000_000_s1"}, 0, sink)
		close(stopped)
	}()
	defer func() { cancel(); <-stopped }() // 先于下一个用例修改退避参数结束
	for i := 1; i <= 3; i++ {
		if i > 1 {
			if _, err := log.Append(ctx, "ses_v1_000_000_s1", uint64(i-1), nil, ev(i)); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case e := <-sink.events:
			if e.GetSeq() != uint64(i) {
				t.Fatalf("got seq %d, want %d", e.GetSeq(), i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never delivered", i)
		}
	}
}

// TestStreamEndsWhenSessionDeleted：删除记录写入后流立即结束（ErrDeleted），不等保活周期的复查。
func TestStreamEndsWhenSessionDeleted(t *testing.T) {
	ctx := context.Background()
	dels := lifecycle.NewMemDeletions()
	done := make(chan error, 1)
	go func() {
		src := Source{Log: memlog.New(), Live: live.Discard{}, Deletions: dels, PingInterval: time.Hour}
		done <- src.Stream(ctx, &session.State{SessionID: "ses_v1_000_000_s1"}, 0, chanSink{events: make(chan *v1.Event, 8)})
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := dels.Mark(ctx, &lifecycle.Tombstone{SessionID: "ses_v1_000_000_s1", BusinessLine: "bl", Reason: "user", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrDeleted) {
			t.Fatalf("stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after the session was deleted")
	}
}

func TestLiveStreamTrace(t *testing.T) {
	for _, outcome := range []string{"done", "canceled", "error"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			tracer, recorder := tracing.NewRecorder()
			const sid, run = "ses_v1_000_000_s1", "run_1"
			log := memlog.New()
			bus := bufferedLive{deltas: make(chan live.Delta, 3)}
			bus.deltas <- live.Delta{SessionID: sid, RunID: run, Attempt: 2, Text: "private"}
			bus.deltas <- live.Delta{SessionID: sid, RunID: run, Attempt: 2, End: true}
			bus.deltas <- live.Delta{SessionID: sid, RunID: run, Attempt: 2, Reasoning: "private"}
			failed := errors.New("sink write failed")
			frames := 0
			sink := traceSink{
				delta: func(live.Delta) error {
					frames++
					if frames < 3 {
						return nil
					}
					switch outcome {
					case "error":
						return failed
					case "canceled":
						cancel()
					case "done":
						_, err := log.Append(ctx, sid, 0, nil, &v1.Event{Payload: &v1.Event_RunCompleted{
							RunCompleted: &v1.RunCompleted{RunId: run, Attempt: 2},
						}})
						return err
					}
					return nil
				},
				event: func(*v1.Event) error { cancel(); return nil },
			}
			src := Source{Log: log, Live: bus, Tracer: tracer}
			st := &session.State{SessionID: sid, Runs: []*session.Run{{ID: run, Attempt: 2, Status: session.RunRunning}}}
			err := src.Stream(ctx, st, 0, sink)
			wantErr, wantCount := error(context.Canceled), int64(2)
			if outcome == "error" {
				wantErr, wantCount = failed, 1
			}
			if !errors.Is(err, wantErr) || frames != 3 {
				t.Fatalf("Stream = %v, sent %d frames", err, frames)
			}
			recs := recorder.Records()
			if len(recs) != 1 {
				t.Fatalf("expected a single Attempt stream span, got %v", recs)
			}
			f := make(map[string]slog.Value)
			recs[0].Attrs(func(a slog.Attr) bool { f[a.Key] = a.Value; return true })
			first := f[tracing.FieldFirstDelta]
			if f[tracing.FieldName].String() != "live.stream" || f[tracing.FieldOutcome].String() != outcome ||
				f[tracing.FieldTraceID].String() != tracing.TraceID(run) || f[tracing.FieldRun].String() != run ||
				f[tracing.FieldSession].String() != sid || f[tracing.FieldAttempt].Uint64() != 2 ||
				f[tracing.FieldDeltas].Int64() != wantCount || first.Kind() != slog.KindFloat64 ||
				first.Float64() < 0 || first.Float64() > f[tracing.FieldDuration].Float64() {
				t.Errorf("live stream span = %v", f)
			}
		})
	}
}

type bufferedLive struct{ deltas chan live.Delta }

func (bufferedLive) Publish(live.Delta) {}
func (b bufferedLive) Subscribe(string) (<-chan live.Delta, func()) {
	return b.deltas, func() {}
}

type traceSink struct {
	chanSink
	delta func(live.Delta) error
	event func(*v1.Event) error
}

func (s traceSink) Delta(d live.Delta) error { return s.delta(d) }
func (s traceSink) Event(e *v1.Event) error  { return s.event(e) }
