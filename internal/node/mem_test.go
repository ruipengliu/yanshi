package node_test

import (
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/node"
	"yanshi/internal/node/nodetest"
)

func TestMemDirectory(t *testing.T) {
	nodetest.RunDirectory(t, func(_ *testing.T, c clock.Clock) node.Directory { return node.NewMemDirectory(c) })
}

func TestMemInbox(t *testing.T) {
	nodetest.RunInbox(t, func(*testing.T) node.Inbox { return node.NewMemInbox() })
}
