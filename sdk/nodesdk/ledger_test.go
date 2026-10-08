package nodesdk_test

import (
	"testing"

	"yanshi/sdk/nodesdk"
	"yanshi/sdk/nodesdk/ledgertest"
)

func TestLedgerConformance(t *testing.T) {
	t.Run("mem", func(t *testing.T) {
		ledgertest.Run(t, func(*testing.T) nodesdk.Ledger { return nodesdk.NewMemLedger() })
	})
	t.Run("file", func(t *testing.T) {
		ledgertest.Run(t, func(t *testing.T) nodesdk.Ledger { return nodesdk.FileLedger{Dir: t.TempDir()} })
	})
}
