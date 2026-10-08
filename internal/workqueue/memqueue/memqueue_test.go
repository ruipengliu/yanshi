package memqueue_test

import (
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/workqueue"
	"yanshi/internal/workqueue/memqueue"
	"yanshi/internal/workqueue/workqueuetest"
)

func TestConformance(t *testing.T) {
	workqueuetest.Run(t, func(_ *testing.T, c clock.Clock) workqueue.Queue { return memqueue.New(c) })
}
