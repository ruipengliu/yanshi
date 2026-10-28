package pgusage_test

import (
	"testing"

	"yanshi/internal/pg/pgtest"
	"yanshi/internal/usage"
	"yanshi/internal/usage/pgusage"
	"yanshi/internal/usage/tiertest"
)

func TestTiers(t *testing.T) {
	tiertest.Run(t, func(t *testing.T) usage.TierStore {
		pool, _ := pgtest.Fresh(t)
		return pgusage.Tiers{Pool: pool}
	})
}
