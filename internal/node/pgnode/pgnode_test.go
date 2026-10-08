package pgnode_test

import (
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/node"
	"yanshi/internal/node/nodetest"
	"yanshi/internal/node/pgnode"
	"yanshi/internal/pg/pgtest"
)

func TestDirectory(t *testing.T) {
	nodetest.RunDirectory(t, func(t *testing.T, c clock.Clock) node.Directory {
		pool, _ := pgtest.Fresh(t)
		return pgnode.NewDirectory(pool, c)
	})
}

func TestInbox(t *testing.T) {
	nodetest.RunInbox(t, func(t *testing.T) node.Inbox { return pgnode.NewInbox(pgtest.Fresh(t)) })
}
