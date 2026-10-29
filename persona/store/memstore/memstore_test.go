package memstore_test

import (
	"persona/store"
	"persona/store/memstore"
	"persona/store/storetest"
	"testing"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func() store.Store { return memstore.New(captureClock) })
}
