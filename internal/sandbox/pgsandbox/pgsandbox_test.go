package pgsandbox_test

import (
	"testing"

	"yanshi/internal/pg/pgtest"
	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/pgsandbox"
	"yanshi/internal/sandbox/sandboxtest"
	"yanshi/sdk/nodesdk"
	"yanshi/sdk/nodesdk/ledgertest"
)

func TestLedger(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T) nodesdk.Ledger {
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
