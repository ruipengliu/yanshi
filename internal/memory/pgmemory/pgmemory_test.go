package pgmemory_test

import (
	"testing"

	"yanshi/internal/memory"
	"yanshi/internal/memory/memorytest"
	"yanshi/internal/memory/pgmemory"
	"yanshi/internal/pg/pgtest"
)

func TestStore(t *testing.T) {
	memorytest.RunStore(t, func(t *testing.T) memory.Store {
		pool, _ := pgtest.Fresh(t)
		return pgmemory.Store{Pool: pool}
	})
}

func TestAccess(t *testing.T) {
	memorytest.RunAccess(t, func(t *testing.T) memory.Store {
		pool, _ := pgtest.Fresh(t)
		return pgmemory.Store{Pool: pool}
	})
}

func TestGrants(t *testing.T) {
	memorytest.RunGrants(t, func(t *testing.T) memory.Grants {
		pool, _ := pgtest.Fresh(t)
		return pgmemory.Grants{Pool: pool}
	})
}
