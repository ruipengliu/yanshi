package memlog_test

import (
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/eventlog/eventlogtest"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/workqueue/memqueue"
)

func TestWaking(t *testing.T) {
	eventlogtest.RunWaking(t, func(_ *testing.T, c clock.Clock) eventlogtest.WakingStores {
		l, q := memlog.New(), memqueue.New(c)
		return eventlogtest.WakingStores{Log: l, Queue: q, Waking: l.Waking(q)}
	})
}
