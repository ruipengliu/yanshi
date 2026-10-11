package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ProviderError 是模型提供商报告的错误。Error() 只含模型、HTTP 状态码与提供商的错误码（经过滤的枚举值），
// 不含错误消息原文：消息可能回显提示词、调用参数或内容片段（个人数据），而错误会写进运维日志与 RunFailed
// （日志只记 ID，ADR-0015）。排查时凭 RequestID 向提供商查询。
type ProviderError struct {
	Model string
	// Status 是 HTTP 状态码；流中途报告的错误为 0。
	Status int
	// Code 是提供商的错误码或类型（ErrorCode），可能为空。
	Code      string
	RequestID string
}

func (e *ProviderError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model %s: ", e.Model)
	if e.Status != 0 {
		fmt.Fprintf(&b, "http %d", e.Status)
	} else {
		b.WriteString("stream error")
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " request %s", e.RequestID)
	}
	return b.String()
}

var unsafeCode = regexp.MustCompile(`[^A-Za-z0-9_.\-]+`)

// SafeCode 把提供商给出的错误码限制为 [A-Za-z0-9_.-]、至多 64 字符，使它可以安全地进入日志。
func SafeCode(s string) string {
	s = unsafeCode.ReplaceAllString(s, "")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// ErrorCode 从提供商的 JSON 错误中取出错误码与类型（形如 {"error":{"code":..,"type":..}}，或直接是错误对象），
// 忽略消息原文；取不到时返回空串。
func ErrorCode(body []byte) string {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	if inner, ok := v["error"].(map[string]any); ok {
		v = inner
	}
	var parts []string
	for _, k := range []string{"code", "type"} {
		switch x := v[k].(type) {
		case string:
			if c := SafeCode(x); c != "" {
				parts = append(parts, c)
			}
		case float64:
			parts = append(parts, fmt.Sprint(int64(x)))
		}
	}
	return strings.Join(parts, "/")
}
