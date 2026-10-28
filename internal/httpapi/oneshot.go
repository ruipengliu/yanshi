package httpapi

import (
	"net/http"

	"yanshi/internal/oneshot"
)

func (s *Server) oneshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BusinessLine string `json:"business_line"`
		EndUser      string `json:"end_user"`
		Input        string `json:"input"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	bl, eu, err := claim(principal(r), req.BusinessLine, req.EndUser)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.Oneshots == nil {
		s.fail(w, oneshot.ErrNotFound)
		return
	}
	result, err := s.Oneshots.Call(r.Context(), oneshot.Request{BusinessLine: bl, EndUser: eu, Name: r.PathValue("name"), Input: req.Input})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
