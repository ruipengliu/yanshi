package pgqueue_test

import (
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/pgqueue"
	"yanshi/internal/workqueue/workqueuetest"
)

func TestConformance(t *testing.T) {
	workqueuetest.Run(t, func(t *testing.T, c clock.Clock) workqueue.Queue {
		pool, _ := pgtest.Fresh(t)
		return pgqueue.New(pool, c)
	})
}
