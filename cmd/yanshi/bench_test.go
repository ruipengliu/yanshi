package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"yanshi/internal/model"
)

func TestBenchFirstBodyDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, delta := range []string{
			`{"run_id":"r","reasoning":"思考"}`,
			`{"run_id":"r","text":""}`,
			`{"run_id":"r","end":true}`,
			`{"run_id":"r","text":"正文"}`,
		} {
			fmt.Fprintf(w, "event: delta\ndata: %s\n\n", delta)
		}
	}))
	defer srv.Close()
	u := &benchUser{}
	out := make(chan benchSignal, 8)
	last := ""
	_ = u.followOnce(context.Background(), srv.URL, "s", &last, out, make(chan struct{}, 1))
	if len(out) != 1 {
		t.Fatalf("body signals: %d", len(out))
	}
	if got := <-out; got.runID != "r" || !got.delta {
		t.Fatalf("signal: %+v", got)
	}
}

func TestBenchFailureAccounting(t *testing.T) {
	start := time.Now()
	r := &benchRecorder{start: start, duration: time.Second}
	r.record(benchRun{submitted: start.Add(-time.Second), status: "completed"})
	for _, status := range []string{"completed", "failed", "timeout", "429", "503"} {
		r.record(benchRun{submitted: start, status: status, ttft: 50 * time.Millisecond, latency: time.Second})
	}
	r.recordShot(start.Add(-time.Second), "completed")
	r.recordShot(start, "completed")
	r.recordShot(start, "429")
	r.recordShot(start, "failed")
	res := r.result(5, 1)
	if res.SuccessRate != 0.2 || res.TTFTSamples != 1 || res.TTFTMs["p95"] != 50 || res.LatencyMs["p99"] != 1000 {
		t.Fatalf("question result: %+v", res)
	}
	if res.Oneshots.Requests != 3 || res.Oneshots.Rejected429Ratio != 1.0/3 || res.Oneshots.SuccessPerSecond != 1 {
		t.Fatalf("oneshots: %+v", res.Oneshots)
	}
}

func TestBenchHTTPFailureClasses(t *testing.T) {
	for _, status := range []int{429, 503, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, "private-content")
		}))
		u := &benchUser{urls: []string{srv.URL}}
		err := u.do(context.Background(), "POST", "/", nil, nil)
		srv.Close()
		want := "failed"
		if status == 429 || status == 503 {
			want = fmt.Sprint(status)
		}
		if benchFailure(err) != want || err.Error() != fmt.Sprintf("HTTP status %d", status) {
			t.Fatal(err)
		}
	}
	if benchFailure(fmt.Errorf("request: %w", context.DeadlineExceeded)) != "timeout" {
		t.Fatal("timeout classification")
	}
}

func TestBenchSubmissionUsesRunTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	r := &benchRecorder{start: time.Now(), duration: time.Minute}
	u := &benchUser{urls: []string{srv.URL}, sid: "s", timeout: 20 * time.Millisecond, rec: r}
	u.runOne(time.Now())
	if got := r.result(1, 1); got.Runs["timeout"] != 1 {
		t.Fatalf("result: %+v", got)
	}
}

func TestBenchWaitIgnoresOtherRunAndKeepsFirstText(t *testing.T) {
	start := time.Now()
	r := &benchRecorder{start: start, duration: time.Minute}
	ch := make(chan benchSignal, 4)
	ch <- benchSignal{runID: "old", delta: true, at: start}
	ch <- benchSignal{runID: "r", delta: true, at: start.Add(50 * time.Millisecond)}
	ch <- benchSignal{runID: "r", delta: true, at: start.Add(100 * time.Millisecond)}
	ch <- benchSignal{runID: "r", status: "completed", at: start.Add(time.Second)}
	r.wait(context.Background(), "r", start, benchRun{submitted: start}, ch)
	if got := r.result(1, 1); got.TTFTMs["p99"] != 50 {
		t.Fatalf("result: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.wait(ctx, "missing", start, benchRun{submitted: start}, ch)
	if got := r.result(1, 1); got.Runs["timeout"] != 1 {
		t.Fatal("missing timeout")
	}
}

func TestBenchConcurrentArrivalsDoNotWaitForCompletion(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]string{"run_id": "r"})
	}))
	defer srv.Close()
	var once sync.Once
	defer once.Do(func() { close(release) })
	r := &benchRecorder{start: time.Now(), duration: time.Minute}
	var work sync.WaitGroup
	var users []*benchUser
	for range 2 {
		u := &benchUser{urls: []string{srv.URL}, sid: "s", timeout: time.Second, rec: r, signals: make(chan benchSignal, 1)}
		users = append(users, u)
	}
	start := time.Now()
	work.Go(func() { benchOpenArrivals(users, start, start.Add(time.Second), 20, &work) })
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(time.Second):
			t.Fatal("arrival waited for previous request")
		}
	}
	once.Do(func() { close(release) })
	for _, u := range users {
		u.signals <- benchSignal{runID: "r", status: "completed", at: time.Now()}
	}
	work.Wait()
}

func TestGatewayBenchUsesGuard(t *testing.T) {
	gw := gateway(slog.New(slog.NewTextHandler(io.Discard, nil)), model.Limits{MaxConcurrency: 1}, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		_, _ = gw.Generate(context.Background(), &model.Request{Model: "bench/latency=0,ttft=0,turns=0"}, func(model.Delta) {
			once.Do(func() {
				close(started)
				<-release
			})
		})
	}()
	defer func() { close(release); <-done }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first bench call did not start")
	}
	// 第一条在回调里持有名额，第二条必须在 Guard 排队到超时，不能触达假模型。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	delta := false
	_, err := gw.Generate(ctx, &model.Request{Model: "bench/latency=0,ttft=0,turns=0"}, func(model.Delta) { delta = true })
	if err == nil || delta {
		t.Fatalf("bench bypassed Guard: delta=%t error=%v", delta, err)
	}
}
