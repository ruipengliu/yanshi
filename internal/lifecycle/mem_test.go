package lifecycle_test

import (
	"testing"

	"yanshi/internal/lifecycle"
	"yanshi/internal/lifecycle/lifecycletest"
)

func TestMemIndex(t *testing.T) {
	lifecycletest.RunIndex(t, func(*testing.T) lifecycle.Index { return lifecycle.NewMemIndex() })
}

func TestMemDeletions(t *testing.T) {
	lifecycletest.RunDeletions(t, func(*testing.T) lifecycle.Deletions { return lifecycle.NewMemDeletions() })
}
