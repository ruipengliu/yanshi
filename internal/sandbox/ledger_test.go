package sandbox_test

import (
	"testing"

	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/sandboxtest"
)

func TestLedger(t *testing.T) {
	sandboxtest.RunLedger(t, func(*testing.T) sandbox.Ledger { return sandbox.NewMemLedger() })
}
