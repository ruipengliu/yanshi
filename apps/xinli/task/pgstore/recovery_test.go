package pgstore_test

import (
	"context"
	"testing"

	"xinli/pg"
	"xinli/pg/pgtest"
	"xinli/recovery/storetest"
	"xinli/task/pgstore"
)

func TestRecovery(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (storetest.Store, func() storetest.Store) {
		pool, dsn := pgtest.NewWithDSN(t)
		return pgstore.New(pool), func() storetest.Store {
			pool.Close()
			reopened, err := pg.Open(context.Background(), dsn, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reopened.Close)
			return pgstore.New(reopened)
		}
	})
}
