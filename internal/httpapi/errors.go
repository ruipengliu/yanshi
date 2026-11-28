package httpapi

import "yanshi/internal/usage"

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
