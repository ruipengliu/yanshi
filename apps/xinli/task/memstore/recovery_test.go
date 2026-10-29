package memstore_test

import (
	"testing"

	"xinli/recovery/storetest"
	"xinli/task/memstore"
)

func TestRecovery(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (storetest.Store, func() storetest.Store) {
		s := memstore.New()
		return s, func() storetest.Store { return s }
	})
}
