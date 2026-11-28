package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"yanshi/internal/lifecycle"
	"yanshi/internal/node"
	"yanshi/internal/service"
)

// delete 的 reason 仅允许显式指定 erasure，且只有服务令牌可以请求（ADR-0050）。
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	reasons, explicit := r.URL.Query()["reason"]
	if explicit {
		if len(reasons) != 1 || reasons[0] != lifecycle.ReasonErasure {
			s.fail(w, fmt.Errorf("%w: unsupported deletion reason", service.ErrInvalid))
			return
		}
		if p := principal(r); !p.Service() && !p.Unrestricted {
			s.fail(w, fmt.Errorf("%w: erasure requires a service token", errForbidden))
			return
		}
	}

	st, ok := s.session(w, r)
	if !ok {
		return
	}
	reason := "end_user"
	if principal(r).Service() {
		reason = "service"
	}
	if explicit {
		reason = lifecycle.ReasonErasure
	}
	if err := s.Service.Delete(r.Context(), st.SessionID, reason, ""); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// nodeRemover 让注销账号同时删除 EndUser 的 Node 登记。
type nodeRemover struct{ dir node.Directory }

func (n nodeRemover) DeleteEndUser(ctx context.Context, businessLine, endUser string) error {
	_, err := n.dir.DeleteScope(ctx, node.Scope{BusinessLine: businessLine, EndUser: endUser})
	return err
}

func (s *Server) deleteEndUser(w http.ResponseWriter, r *http.Request) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return
	}
	var removers []service.UserDataRemover
	if s.Nodes != nil {
		removers = append(removers, nodeRemover{s.Nodes})
	}
	if s.Memory != nil {
		removers = append(removers, s.Memory)
	}
	if s.Usage != nil {
		removers = append(removers, s.Usage) // 匿名化，不删除（ADR-0019）
	}
	if s.Push != nil {
		removers = append(removers, s.Push)
	}
	id, err := s.Service.DeleteEndUser(r.Context(), bl, r.PathValue("id"), removers...)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"request_id": id})
}

// sessionDeletion 只读 Tombstone：Session 日志已清除时仍可按业务线查进度。
func (s *Server) sessionDeletion(w http.ResponseWriter, r *http.Request) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return
	}
	if s.Service.Deletions == nil {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Error: "deletion is not configured", Code: codeNotImplemented})
		return
	}
	tomb, err := s.Service.Deletions.Get(r.Context(), r.PathValue("id"))
	if err == nil && tomb.BusinessLine != bl {
		err = lifecycle.ErrNotFound
	}
	if errors.Is(err, lifecycle.ErrNotFound) {
		err = service.ErrNotFound
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"reason": tomb.Reason, "requested_at": tomb.RequestedAt}
	if !tomb.CompletedAt.IsZero() {
		out["completed_at"] = tomb.CompletedAt
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deletion(w http.ResponseWriter, r *http.Request) {
	bl, ok := businessLineOf(w, r, s)
	if !ok {
		return
	}
	if s.Service.Deletions == nil {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Error: "deletion is not configured", Code: codeNotImplemented})
		return
	}
	req, err := s.Service.DeletionRequest(r.Context(), bl, r.PathValue("id"))
	if errors.Is(err, lifecycle.ErrNotFound) {
		err = fmt.Errorf("%w: deletion %s", service.ErrNotFound, r.PathValue("id"))
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": req.ID, "created_at": req.CreatedAt, "sessions": req.Sessions, "completed": req.Completed,
		"done": req.Completed == req.Sessions,
	})
}
