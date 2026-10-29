package mcpserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Credential 绑定可信平台与业务线；轮换期间可同时登记多把，期限逐请求检查。
type Credential struct {
	BusinessLine string    `json:"business_line"`
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

var errUnauthorized = errors.New("persona: unauthorized")

func ValidateCredentials(credentials []Credential) error {
	seen := map[string]bool{}
	for _, c := range credentials {
		if c.BusinessLine == "" || c.Token == "" || strings.ContainsAny(c.Token, " \t\r\n") || c.ExpiresAt.IsZero() || seen[c.Token] {
			return errors.New("persona: invalid service credentials")
		}
		seen[c.Token] = true
	}
	if len(credentials) == 0 {
		return errors.New("persona: service credentials required")
	}
	return nil
}

func authenticate(r *http.Request, credentials []Credential, now time.Time) string {
	header := r.Header.Get("Authorization")
	if len(r.Header.Values("Authorization")) != 1 || len(header) <= 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return ""
	}
	digest := sha256.Sum256([]byte(header[7:]))
	line := ""
	for _, c := range credentials {
		want := sha256.Sum256([]byte(c.Token))
		if subtle.ConstantTimeCompare(digest[:], want[:]) == 1 && now.Before(c.ExpiresAt) {
			line = c.BusinessLine
		}
	}
	return line
}

func (x *server) authorize(metadata mcp.Meta) error {
	if x.businessLine == "" {
		return nil // 只有显式开发入口创建不绑定业务线的 Server。
	}
	bl, _ := metadata["yanshi/business_line"].(string)
	eu, _ := metadata["yanshi/end_user"].(string)
	sid, _ := metadata["yanshi/session"].(string)
	if bl != x.businessLine || eu == "" || sid == "" {
		return errUnauthorized
	}
	return nil
}
