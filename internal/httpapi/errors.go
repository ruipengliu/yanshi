package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
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

// 错误码是 HTTP API 的公开契约，已发布的值不能改名或复用。
const (
	codeInvalidArgument       = "invalid_argument"
	codeUnauthenticated       = "unauthenticated"
	codeForbidden             = "forbidden"
	codeNotFound              = "not_found"
	codeConflict              = "conflict"
	codeGone                  = "gone"
	codeTooLarge              = "too_large"
	codeModerationRejected    = "moderation_rejected"
	codeMemoryRejected        = "memory_rejected"
	codeMemoryLimit           = "memory_limit"
	codeQuotaExceeded         = "quota_exceeded"
	codeRateLimited           = "rate_limited"
	codeInternal              = "internal"
	codeNotImplemented        = "not_implemented"
	codeProviderError         = "provider_error"
	codeShardUnavailable      = "shard_unavailable"
	codeModerationUnavailable = "moderation_unavailable"
	codeOneshotUnavailable    = "oneshot_unavailable"
	codeOutboxUnavailable     = "outbox_unavailable"
	codeOverloaded            = "overloaded"
	codeScanBudgetExceeded    = "scan_budget_exceeded"
	codeTimeout               = "timeout"
)

// 出站流的客户端据此选择恢复路径，错误文字可独立演进。
const (
	reasonOutboxFilterChanged = "outbox_filter_changed"
	reasonOutboxEpochChanged  = "outbox_epoch_changed"
	reasonOutboxCursorExpired = "outbox_cursor_expired"
)

type errorResponse struct {
	Error  string        `json:"error"`
	Code   string        `json:"code"`
	Reason string        `json:"reason,omitempty"`
	Quota  *usage.Period `json:"quota,omitempty"`
}

// errForbidden 用于调用方自报的身份与令牌不符；访问别人的资源则返回 404，不泄露其存在。
var errForbidden = errors.New("forbidden")

func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := errorResponse{Code: codeInternal}
	switch {
	case errors.Is(err, shard.ErrUnavailable):
		status, body.Code = http.StatusServiceUnavailable, codeShardUnavailable
	case errors.Is(err, service.ErrNotFound), errors.Is(err, oneshot.ErrNotFound), errors.Is(err, shard.ErrInvalidID):
		// 无法解析的 ID 不可能指向任何对象，按不存在处理。
		status, body.Code = http.StatusNotFound, codeNotFound
	case errors.Is(err, errForbidden):
		status, body.Code = http.StatusForbidden, codeForbidden
	case errors.Is(err, service.ErrInvalid), errors.Is(err, oneshot.ErrInvalid):
		status, body.Code = http.StatusBadRequest, codeInvalidArgument
	case errors.Is(err, service.ErrConflict):
		status, body.Code = http.StatusConflict, codeConflict
	case errors.Is(err, service.ErrScanBudgetExceeded):
		status, body.Code = http.StatusUnprocessableEntity, codeScanBudgetExceeded
	case errors.Is(err, artifact.ErrNotFound):
		status, body.Code = http.StatusNotFound, codeNotFound
	case errors.Is(err, memory.ErrNotFound):
		status, body.Code = http.StatusNotFound, codeNotFound
	case errors.Is(err, memory.ErrRejected):
		status, body.Code = http.StatusBadRequest, codeMemoryRejected
	case errors.Is(err, memory.ErrLimit):
		status, body.Code = http.StatusConflict, codeMemoryLimit
	case errors.Is(err, artifact.ErrTooLarge):
		status, body.Code = http.StatusRequestEntityTooLarge, codeTooLarge
	case errors.Is(err, moderation.ErrRejected):
		status, body.Code = http.StatusUnprocessableEntity, codeModerationRejected
	case errors.Is(err, speech.ErrNotConfigured):
		status, body.Code = http.StatusNotImplemented, codeNotImplemented
	case errors.Is(err, speech.ErrProvider):
		status, body.Code = http.StatusBadGateway, codeProviderError
	case errors.Is(err, moderation.ErrUnavailable):
		status, body.Code = http.StatusServiceUnavailable, codeModerationUnavailable
	case errors.Is(err, model.ErrBusy):
		status, body.Code = http.StatusTooManyRequests, codeRateLimited
	case errors.Is(err, context.DeadlineExceeded):
		status, body.Code = http.StatusGatewayTimeout, codeTimeout
	case errors.Is(err, oneshot.ErrProvider), errors.Is(err, oneshot.ErrSchema):
		status, body.Code = http.StatusBadGateway, codeProviderError
	case errors.Is(err, oneshot.ErrInternal):
		status, body.Code = http.StatusServiceUnavailable, codeOneshotUnavailable
	case errors.Is(err, service.ErrOverloaded):
		status, body.Code = http.StatusServiceUnavailable, codeOverloaded
		var ov *service.OverloadedError
		if errors.As(err, &ov) {
			w.Header().Set("Retry-After", strconv.Itoa(max(int(math.Ceil(time.Until(ov.RetryAt).Seconds())), 1)))
		}
	case errors.Is(err, usage.ErrExceeded):
		status, body.Code = http.StatusTooManyRequests, codeQuotaExceeded
		var ex *usage.ExceededError
		if errors.As(err, &ex) {
			secs := int(math.Ceil(time.Until(ex.Period.ResetAt).Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
			body.Quota = ex.Period
		}
	}
	_, sharded := s.Outbox.(outbox.Shards)
	if body.Code == codeShardUnavailable || sharded && (status == http.StatusInternalServerError || status == http.StatusGatewayTimeout) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "shard_unavailable", Code: codeShardUnavailable})
		return
	}
	if status == http.StatusInternalServerError && s.Logger != nil {
		s.Logger.Error("request failed", "err", err)
	}
	body.Error = err.Error()
	var reason *service.ReasonError
	if errors.As(err, &reason) {
		body.Reason = reason.Reason
	}
	writeJSON(w, status, body)
}
