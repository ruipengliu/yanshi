package httpapi

import (
	"fmt"
	"net/http"

	"yanshi/internal/service"
)

func (s *Server) putTier(w http.ResponseWriter, r *http.Request) {
	// 先鉴权再读请求，用户令牌无论请求内容如何都不能管理档位。
	p := principal(r)
	if !p.Unrestricted && !p.Service() {
		s.fail(w, fmt.Errorf("%w: a service token is required", errForbidden))
		return
	}
	var req struct {
		BusinessLine string `json:"business_line"`
		Tier         string `json:"tier"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	// 开发模式兼容注销接口的查询参数，也允许 PUT 在 JSON 中给出业务线。
	queryLine := r.URL.Query().Get("business_line")
	if req.BusinessLine != "" && queryLine != "" && req.BusinessLine != queryLine {
		s.fail(w, fmt.Errorf("%w: conflicting business_line", service.ErrInvalid))
		return
	}
	bl := req.BusinessLine
	if bl == "" {
		bl = queryLine
	}
	if !p.Unrestricted {
		if bl != "" && bl != p.BusinessLine {
			s.fail(w, fmt.Errorf("%w: business_line does not match token", errForbidden))
			return
		}
		bl = p.BusinessLine
	}
	status, err := s.Service.PutTier(r.Context(), bl, r.PathValue("id"), req.Tier)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) deleteTier(w http.ResponseWriter, r *http.Request) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return
	}
	if requested := r.URL.Query().Get("business_line"); requested != "" && requested != bl {
		s.fail(w, fmt.Errorf("%w: business_line does not match token", errForbidden))
		return
	}
	status, err := s.Service.DeleteTier(r.Context(), bl, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
