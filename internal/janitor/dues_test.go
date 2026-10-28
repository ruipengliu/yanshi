package janitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/sandbox"
	"yanshi/internal/workqueue"
)

type dueQueue struct {
	workqueue.Queue
	clock   *clock.Fake
	step    time.Duration
	befores []time.Time
	limit   int
	full    bool
	err     error
}

func (q *dueQueue) Horizon() time.Duration { return time.Hour }
func (q *dueQueue) Promote(_ context.Context, before time.Time, limit int) (int, error) {
	q.befores = append(q.befores, before)
	q.limit = limit
	q.clock.Advance(q.step)
	if q.err != nil {
		return 3, q.err
	}
	if q.full || len(q.befores) == 1 {
		return limit, nil
	}
	return 2, nil
}

func TestSweepPromotesDues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		step   time.Duration
		full   bool
		fail   bool
		calls  int
	}{
		{name: "partial", step: time.Second, calls: 2},
		// 默认预算为一分钟的一半；连续满批可以超过原来的十批上限。
		{name: "default_budget", step: time.Second, full: true, calls: 30},
		// 在批次边界停止，不打断最后一批的提交。
		{name: "custom_budget", budget: 5 * time.Second, step: 2 * time.Second, full: true, calls: 3},
		{name: "error", step: time.Second, fail: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			start := e.clk.Now()
			q := &dueQueue{Queue: e.stors.Queue, clock: e.clk, step: tc.step, full: tc.full}
			if tc.fail {
				q.err = errors.New("promotion failed")
			}
			j := e.core.Janitor("j", sandbox.NewFake(), nil, 0)
			j.Queues = map[string]workqueue.Queue{"sessions": q}
			j.PromoteBudget = tc.budget
			err := j.Sweep(context.Background())
			if !errors.Is(err, q.err) {
				t.Fatalf("sweep = %v, want %v", err, q.err)
			}
			if len(q.befores) != tc.calls || q.limit != 1000 {
				t.Fatalf("calls=%d limit=%d", len(q.befores), q.limit)
			}
			for i, before := range q.befores {
				if want := start.Add(time.Hour + time.Duration(i)*tc.step); !before.Equal(want) {
					t.Fatalf("batch %d before=%s, want %s", i, before, want)
				}
			}
			if tc.budget == 0 && j.PromoteBudget != j.SweepEvery/2 {
				t.Fatalf("default budget=%s sweep=%s", j.PromoteBudget, j.SweepEvery)
			}
		})
	}
}
