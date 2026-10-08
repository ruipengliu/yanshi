package memlog_test

import (
	"testing"

	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/eventlogtest"
	"yanshi/internal/eventlog/memlog"
)

func TestConformance(t *testing.T) {
	eventlogtest.Run(t, func(*testing.T) eventlog.Log { return memlog.New() })
}
