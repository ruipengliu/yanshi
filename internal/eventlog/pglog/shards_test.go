package pglog_test

import (
	"context"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/pgqueue"
)

func TestWakingRejectsAnotherDatabase(t *testing.T) {
	p0, n0 := pgtest.FreshDatabase(t)
	p1, _ := pgtest.FreshDatabase(t)
	log := pglog.New(p0, n0)
	queue := pgqueue.New(p1, clock.Real{}, pgqueue.Sessions)
	if _, err := log.Waking(queue).AppendAndEnqueue(context.Background(), "s", 0, workqueue.Class{}, nil, &v1.Event{}); err == nil {
		t.Fatal("cross-database waking accepted")
	}
	if rows, err := log.Read(context.Background(), "s", 0, 10); err != nil || len(rows) != 0 {
		t.Fatal("rejected waking wrote events")
	}
	if has, err := queue.Contains(context.Background(), "s"); err != nil || has {
		t.Fatal("rejected waking enqueued work")
	}
}
