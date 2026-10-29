package memstore_test

import (
	"testing"

	"xinli/task"
	"xinli/task/memstore"
	"xinli/task/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (task.Store, func() task.Store) {
		s := memstore.New()
		return s, func() task.Store { return s }
	})
}
