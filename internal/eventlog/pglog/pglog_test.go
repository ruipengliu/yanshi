package pglog_test

import (
	"testing"

	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/eventlogtest"
	"yanshi/internal/eventlog/pglog"
	"yanshi/internal/pg/pgtest"
)

func TestConformance(t *testing.T) {
	eventlogtest.Run(t, func(t *testing.T) eventlog.Log { return pglog.New(pgtest.Fresh(t)) })
}
