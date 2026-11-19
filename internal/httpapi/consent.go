package httpapi

import (
	"fmt"
	"net/http"
	"strconv"

	"yanshi/internal/consent"
	"yanshi/internal/memory"
	"yanshi/internal/service"
)

// putConsent 登记 EndUser 的单独同意（ADR-0022 修订）：业务线服务端在用户同意后调用，只接受服务令牌，业务线取自令牌
// （开发模式取自查询参数，同 DELETE /v1/end_users/{id}/tier）。
func (s *Server) putConsent(w http.ResponseWriter, r *http.Request) {
	bl, ok := s.consentBusinessLine(w, r)
	if !ok {
		return
	}
	status, err := s.Service.PutConsent(r.Context(), bl, r.PathValue("id"), consent.Kind(r.PathValue("kind")))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// deleteConsent 撤回同意，立即停止使用；purge=true 时一并删除该用户在本业务线的健康 Memory。先撤回、再删除：
// 删除之后不会再有本进程的新写入（其他进程至多 consent.CacheTTL 后停止写入）。
func (s *Server) deleteConsent(w http.ResponseWriter, r *http.Request) {
	bl, ok := s.consentBusinessLine(w, r)
	if !ok {
		return
	}
	purge := false
	if v := r.URL.Query().Get("purge"); v != "" {
		var err error
		if purge, err = strconv.ParseBool(v); err != nil {
			s.fail(w, fmt.Errorf("%w: purge must be true or false", service.ErrInvalid))
			return
		}
	}
	if purge && s.Memory == nil {
		s.fail(w, fmt.Errorf("%w: memory is not configured", service.ErrInvalid))
		return
	}
	eu, kind := r.PathValue("id"), consent.Kind(r.PathValue("kind"))
	status, err := s.Service.DeleteConsent(r.Context(), bl, eu, kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	if purge {
		// consent.Valid 只接受 health；新增类别时在这里对应要清除的数据。
		n, err := s.Memory.ForgetCategory(r.Context(), bl, eu, memory.Health)
		if err != nil {
			s.fail(w, err)
			return
		}
		status.Purged = &n
	}
	writeJSON(w, http.StatusOK, status)
}

// consentBusinessLine 先鉴权再读请求，用户令牌无论请求内容如何都不能管理同意；查询参数中的业务线须与令牌一致。
func (s *Server) consentBusinessLine(w http.ResponseWriter, r *http.Request) (string, bool) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return "", false
	}
	if requested := r.URL.Query().Get("business_line"); requested != "" && requested != bl {
		s.fail(w, fmt.Errorf("%w: business_line does not match token", errForbidden))
		return "", false
	}
	return bl, true
}
