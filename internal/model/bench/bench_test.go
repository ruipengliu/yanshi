package bench

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/model"
)

func TestTimingParameters(t *testing.T) {
	for _, tc := range []struct {
		id           string
		first, total time.Duration
	}{
		{"latency=1s,ttft=50ms,turns=0", 50 * time.Millisecond, time.Second},
		{"latency=800ms", 100 * time.Millisecond, 800 * time.Millisecond},
		{"latency=0,ttft=0", 0, 0},
	} {
		p, err := parse(tc.id)
		if err != nil || p.ttft != tc.first || p.latency != tc.total {
			t.Fatalf("parse %s: %+v %v", tc.id, p, err)
		}
	}
	for _, id := range []string{"latency=1s,ttft=2s", "latency=-1s", "ttft=-1ms"} {
		if _, err := parse(id); err == nil {
			t.Fatalf("accepted %s", id)
		}
	}
}

func TestFirstDeltaPrecedesTotalLatency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := 0
	_, err := (Provider{}).Generate(ctx, &model.Request{Model: "latency=1h,ttft=0,turns=0"}, func(d model.Delta) {
		if d.Text != "" {
			count++
			cancel()
		}
	})
	if err != context.Canceled || count != 1 {
		t.Fatalf("delta count=%d error=%v", count, err)
	}
}
