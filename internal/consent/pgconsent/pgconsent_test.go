package pgconsent_test

import (
	"testing"

	"yanshi/internal/consent"
	"yanshi/internal/consent/consenttest"
	"yanshi/internal/consent/pgconsent"
	"yanshi/internal/pg/pgtest"
)

func TestStore(t *testing.T) {
	consenttest.Run(t, func(t *testing.T) consent.Store {
		pool, _ := pgtest.Fresh(t)
		return pgconsent.Store{Pool: pool}
	})
}
