package pglifecycle_test

import (
	"testing"

	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/lifecycletest"
	"yanshi/internal/lifecycle/pglifecycle"
	"yanshi/internal/pg/pgtest"
)

func TestIndex(t *testing.T) {
	lifecycletest.RunIndex(t, func(t *testing.T) lifecycle.Index {
		pool, _ := pgtest.Fresh(t)
		return pglifecycle.Index{Pool: pool}
	})
}

func TestDeletions(t *testing.T) {
	lifecycletest.RunDeletions(t, func(t *testing.T) lifecycle.Deletions {
		pool, n := pgtest.Fresh(t)
		return pglifecycle.Deletions{Pool: pool, Notifier: n}
	})
}
