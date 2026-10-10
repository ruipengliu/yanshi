package pgpresence_test

import (
	"testing"

	"yanshi/internal/pg/pgtest"
	"yanshi/internal/presence"
	"yanshi/internal/presence/pgpresence"
	"yanshi/internal/presence/presencetest"
)

func TestStore(t *testing.T) {
	presencetest.Run(t, func(t *testing.T) presence.Store {
		pool, n := pgtest.Fresh(t)
		return pgpresence.Store{Pool: pool, Notifier: n}
	})
}
