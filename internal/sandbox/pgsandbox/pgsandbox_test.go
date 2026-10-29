package pgsandbox_test

import (
	"testing"

	"yanshi/internal/pg/pgtest"
	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/sandbox/sandboxtest"
)

func TestLedger(t *testing.T) {
	sandboxtest.RunLedger(t, func(t *testing.T) sandbox.Ledger {
		pool, _ := pgtest.Fresh(t)
		return pgsandbox.Ledger{Pool: pool}
	})
}

func TestActivity(t *testing.T) {
	sandboxtest.RunActivity(t, func(t *testing.T) sandbox.Activity {
		pool, _ := pgtest.Fresh(t)
		return pgsandbox.Activity{Pool: pool}
	})
}
