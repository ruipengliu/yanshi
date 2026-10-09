package pgsnapshot_test

import (
	"testing"

	"yanshi/internal/pg/pgtest"
	"yanshi/internal/session"
	"yanshi/internal/session/pgsnapshot"
	"yanshi/internal/session/snapshottest"
)

func TestStore(t *testing.T) {
	snapshottest.Run(t, func(t *testing.T) session.Snapshots {
		pool, _ := pgtest.Fresh(t)
		return pgsnapshot.Store{Pool: pool}
	})
}
