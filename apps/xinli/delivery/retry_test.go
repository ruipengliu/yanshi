package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"xinli/kernel"
	"xinli/recovery"
)

type sqlFailure string

func (e sqlFailure) Error() string    { return "private statement and parameter" }
func (e sqlFailure) SQLState() string { return string(e) }

func TestFailureReportsStageShardAndSafeCategory(t *testing.T) {
	for _, test := range []struct {
		err      error
		category string
	}{
		{recovery.ErrConflict, "progress_conflict"},
		{context.DeadlineExceeded, "timeout"},
		{&kernel.HTTPError{StatusCode: 503, Reason: "private downstream detail"}, "kernel_http_503"},
		{sqlFailure("23505"), "sql_23505"},
		{sqlFailure("private"), "operation_failed"},
		{errors.New("private body"), "operation_failed"},
	} {
		detail := retryDetail{cause: test.err, phase: recovery.Events, session: "session", run: "run", envelope: "envelope"}
		f := failureInfo("recovery", "bl", 1, detail)
		if f.Category != test.category || f.Stage != "recovery" || f.Shard != 1 || f.Phase != recovery.Events || f.SessionID != "session" || f.RunID != "run" || f.EnvelopeID != "envelope" {
			t.Fatalf("missing retry identity: %+v", f)
		}
		raw, err := json.Marshal(f)
		if err != nil || strings.Contains(string(raw), "private") {
			t.Fatalf("retry leaked error text: %s %v", raw, err)
		}
	}
}

func TestFailureReportsEachReceipt(t *testing.T) {
	err := errors.Join(
		retryDetail{cause: context.DeadlineExceeded, session: "first", envelope: "first-envelope"},
		retryDetail{cause: sqlFailure("23505"), session: "second", envelope: "second-envelope"},
	)
	infos := failureInfos("process", "bl", -1, err)
	if len(infos) != 2 || infos[0].Category != "timeout" || infos[0].SessionID != "first" || infos[0].EnvelopeID != "first-envelope" || infos[1].Category != "sql_23505" || infos[1].SessionID != "second" || infos[1].EnvelopeID != "second-envelope" {
		t.Fatalf("receipt identity and error category mixed: %+v", infos)
	}
}
