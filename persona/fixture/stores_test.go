package fixture_test

import (
	"testing"

	"persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

func fixtureStores(t *testing.T, test func(*testing.T, store.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, memstore.New()) })
	t.Run("pg", func(t *testing.T) { test(t, pgstore.New(pgtest.New(t))) })
}
