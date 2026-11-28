package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"yanshi/internal/artifact"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/oneshot"
	"yanshi/internal/outbox"
	"yanshi/internal/service"
	"yanshi/internal/shard"
	"yanshi/internal/speech"
	"yanshi/internal/usage"
)

type errorTestShards struct {
	outbox.Reader
	outbox.Shards
}

func TestFailErrorResponse(t *testing.T) {
	period := &usage.Period{Scope: "end_user", ResetAt: time.Unix(0, 0).UTC(), Exceeded: true}
	redo := &service.ReasonError{Err: fmt.Errorf("%w: redo: not_last_run", service.ErrConflict), Reason: "not_last_run"}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", service.ErrNotFound, 404, "not_found"},
		{"oneshot not found", oneshot.ErrNotFound, 404, "not_found"},
		{"invalid ID", shard.ErrInvalidID, 404, "not_found"},
		{"artifact not found", artifact.ErrNotFound, 404, "not_found"},
		{"memory not found", memory.ErrNotFound, 404, "not_found"},
		{"forbidden", errForbidden, 403, "forbidden"},
		{"invalid", service.ErrInvalid, 400, "invalid_argument"},
		{"oneshot invalid", oneshot.ErrInvalid, 400, "invalid_argument"},
		{"conflict", service.ErrConflict, 409, "conflict"},
		{"redo", redo, 409, "conflict"},
		{"memory rejected", memory.ErrRejected, 400, "memory_rejected"},
		{"memory limit", memory.ErrLimit, 409, "memory_limit"},
		{"too large", artifact.ErrTooLarge, 413, "too_large"},
		{"rejected", moderation.ErrRejected, 422, "moderation_rejected"},
		{"not implemented", speech.ErrNotConfigured, 501, "not_implemented"},
		{"speech provider", speech.ErrProvider, 502, "provider_error"},
		{"oneshot provider", oneshot.ErrProvider, 502, "provider_error"},
		{"oneshot schema", oneshot.ErrSchema, 502, "provider_error"},
		{"moderation unavailable", moderation.ErrUnavailable, 503, "moderation_unavailable"},
		{"oneshot unavailable", oneshot.ErrInternal, 503, "oneshot_unavailable"},
		{"shard unavailable", shard.ErrUnavailable, 503, "shard_unavailable"},
		{"rate limited", model.ErrBusy, 429, "rate_limited"},
		{"quota sentinel", usage.ErrExceeded, 429, "quota_exceeded"},
		{"quota with period", &usage.ExceededError{Period: period}, 429, "quota_exceeded"},
		{"overloaded", &service.OverloadedError{RetryAt: time.Unix(0, 0)}, 503, "overloaded"},
		{"timeout", context.DeadlineExceeded, 504, "timeout"},
		{"internal", errors.New("request failed"), 500, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).fail(w, tc.err)
			want := errorResponse{Error: tc.err.Error(), Code: tc.code}
			var retryAfter string
			switch tc.name {
			case "redo":
				want.Reason = "not_last_run"
			case "quota with period":
				want.Quota, retryAfter = period, "1"
			case "overloaded", "shard unavailable":
				retryAfter = "1"
			}
			assertErrorResponse(t, w, tc.status, want, retryAfter)
		})
	}
}

func TestFailShardedMasksErrorText(t *testing.T) {
	for _, err := range []error{fmt.Errorf("%w: storage detail", shard.ErrUnavailable), context.DeadlineExceeded, errors.New("storage detail")} {
		w := httptest.NewRecorder()
		(&Server{Outbox: errorTestShards{}}).fail(w, err)
		assertErrorResponse(t, w, 503, errorResponse{Error: "shard_unavailable", Code: "shard_unavailable"}, "1")
	}
}

func TestOutboxErrorResponse(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		body   errorResponse
		retry  string
	}{
		{outbox.ErrInvalid, 400, errorResponse{Error: "outbox_invalid_request", Code: "invalid_argument"}, ""},
		{outbox.ErrCursor, 400, errorResponse{Error: "outbox_invalid_cursor", Code: "invalid_argument"}, ""},
		{outbox.ErrScope, 403, errorResponse{Error: "outbox_cursor_scope_mismatch", Code: "forbidden"}, ""},
		{outbox.ErrShard, 404, errorResponse{Error: "outbox_unknown_shard", Code: "not_found"}, ""},
		{outbox.ErrFilter, 409, errorResponse{Error: "outbox_filter_changed", Code: "conflict", Reason: "outbox_filter_changed"}, ""},
		{outbox.ErrEpoch, 410, errorResponse{Error: "outbox_epoch_changed", Code: "gone", Reason: "outbox_epoch_changed"}, ""},
		{outbox.ErrExpired, 410, errorResponse{Error: "outbox_cursor_expired", Code: "gone", Reason: "outbox_cursor_expired"}, ""},
		{errors.New("storage detail"), 503, errorResponse{Error: "outbox_unavailable", Code: "outbox_unavailable"}, "1"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).outboxError(w, tc.err)
			assertErrorResponse(t, w, tc.status, tc.body, tc.retry)
		})
	}
}

func assertErrorResponse(t *testing.T, w *httptest.ResponseRecorder, status int, want errorResponse, retryAfter string) {
	t.Helper()
	wantBody := map[string]any{"error": want.Error, "code": want.Code}
	if want.Reason != "" {
		wantBody["reason"] = want.Reason
	}
	if want.Quota != nil {
		wantBody["quota"] = want.Quota
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(body)
	wantJSON, _ := json.Marshal(wantBody)
	if w.Code != status || string(gotJSON) != string(wantJSON) || w.Header().Get("Retry-After") != retryAfter {
		t.Fatalf("got %d %s Retry-After=%q, want %d %s Retry-After=%q", w.Code, w.Body, w.Header().Get("Retry-After"), status, wantJSON, retryAfter)
	}
}
