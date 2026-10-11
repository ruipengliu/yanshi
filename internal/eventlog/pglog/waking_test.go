package pglog_test

import (
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/eventlog/eventlogtest"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/workqueue/pgqueue"
)

func TestWaking(t *testing.T) {
	eventlogtest.RunWaking(t, func(t *testing.T, c clock.Clock) eventlogtest.WakingStores {
		pool, n := pgtest.Fresh(t)
		l, q := pglog.New(pool, n), pgqueue.New(pool, c, pgqueue.Sessions).WithNotifier(n)
		return eventlogtest.WakingStores{Log: l, Queue: q, Waking: l.Waking(q)}
	})
}
