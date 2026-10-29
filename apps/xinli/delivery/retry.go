package delivery

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"xinli/kernel"
	"xinli/recovery"
)

// Failure 只携带固定类别、阶段和 ID，不暴露下游错误文本或不透明游标。
type Failure struct {
	Stage, BusinessLine, Category, Phase string
	Shard                                int
	SessionID, RunID, CallID, EnvelopeID string
}

type retryDetail struct {
	cause                               error
	phase, session, run, call, envelope string
}

func (e retryDetail) Error() string { return e.cause.Error() }
func (e retryDetail) Unwrap() error { return e.cause }

func failureInfos(stage, line string, shard int, err error) []Failure {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []Failure
		for _, child := range joined.Unwrap() {
			out = append(out, failureInfos(stage, line, shard, child)...)
		}
		return out
	}
	return []Failure{failureInfo(stage, line, shard, err)}
}

func failureInfo(stage, line string, shard int, err error) Failure {
	f := Failure{Stage: stage, BusinessLine: line, Shard: shard, Category: "operation_failed"}
	var detail retryDetail
	if errors.As(err, &detail) {
		f.Phase, f.SessionID, f.RunID, f.CallID, f.EnvelopeID = detail.phase, detail.session, detail.run, detail.call, detail.envelope
	}
	var http *kernel.HTTPError
	var sql interface{ SQLState() string }
	switch {
	case errors.Is(err, recovery.ErrConflict):
		f.Category = "progress_conflict"
	case errors.Is(err, context.DeadlineExceeded):
		f.Category = "timeout"
	case errors.Is(err, context.Canceled):
		f.Category = "canceled"
	case errors.As(err, &http):
		f.Category = "kernel_http_" + strconv.Itoa(http.StatusCode)
	case errors.As(err, &sql):
		code := sql.SQLState()
		if len(code) == 5 && strings.Trim(code, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			f.Category = "sql_" + code
		}
	}
	return f
}
