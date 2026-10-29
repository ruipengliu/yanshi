package pgstore_test

import (
	"context"
	"testing"

	"xinli/pg"
	"xinli/pg/pgtest"
	"xinli/task"
	"xinli/task/pgstore"
	"xinli/task/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (task.Store, func() task.Store) {
		pool, dsn := pgtest.NewWithDSN(t)
		return pgstore.New(pool), func() task.Store {
			pool.Close()
			reopened, err := pg.Open(context.Background(), dsn, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reopened.Close)
			if err := pg.Migrate(context.Background(), reopened); err != nil {
				t.Fatal(err)
			}
			return pgstore.New(reopened)
		}
	})
}
