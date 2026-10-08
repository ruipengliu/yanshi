package sandbox_test

import (
	"testing"

	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/sandboxtest"
)

func TestMemActivity(t *testing.T) {
	sandboxtest.RunActivity(t, func(*testing.T) sandbox.Activity { return sandbox.NewMemActivity() })
}
