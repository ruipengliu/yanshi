package pgusage_test

import (
	"testing"

	"yanshi/internal/pg/pgtest"
	"yanshi/internal/usage"
	"yanshi/internal/usage/pgusage"
	"yanshi/internal/usage/usagetest"
)

func TestStore(t *testing.T) {
	usagetest.Run(t, func(t *testing.T) usage.Store {
		pool, _ := pgtest.Fresh(t)
		return pgusage.Store{Pool: pool}
	})
}
