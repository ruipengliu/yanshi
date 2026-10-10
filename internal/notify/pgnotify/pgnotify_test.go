package pgnotify_test

import (
	"testing"

	"yanshi/internal/notify"
	"yanshi/internal/notify/notifytest"
	"yanshi/internal/notify/pgnotify"
	"yanshi/internal/pg/pgtest"
)

func TestRegistry(t *testing.T) {
	notifytest.Run(t, func(t *testing.T) notify.Registry {
		pool, _ := pgtest.Fresh(t)
		return pgnotify.Registry{Pool: pool}
	})
}
