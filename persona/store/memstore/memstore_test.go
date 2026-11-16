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

// 内存实现默认整体装载；打开 PartialReads 时执行同一读取计划，在没有 PG 时检查计划本身。
func TestPartialReadPlans(t *testing.T) {
	s := memstore.New(captureClock)
	s.PartialReads = true
	storetest.PartialReads(t, s, true)
}
