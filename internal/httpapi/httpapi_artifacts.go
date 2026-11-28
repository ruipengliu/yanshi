package httpapi

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"yanshi/internal/artifact"
)

// artifact 读取工件元数据并要求其所属 Session 属于调用方。
func (s *Server) artifact(w http.ResponseWriter, r *http.Request) (*artifact.Meta, bool) {
	if !s.artifacts(w) {
		return nil, false
	}
	id := r.PathValue("id")
	m, err := s.Artifacts.Lookup(r.Context(), id)
	if err == nil {
		st, lerr := s.Service.Load(r.Context(), m.SessionID)
		if lerr != nil || !principal(r).Allows(st.Created.GetBusinessLine(), st.Created.GetEndUser()) {
			err = fmt.Errorf("%w: artifact %s", artifact.ErrNotFound, id)
		}
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return m, true
}

type artifactView struct {
	ID        string    `json:"id"`
	URI       string    `json:"uri"`
	SessionID string    `json:"session_id"`
	Name      string    `json:"name"`
	MimeType  string    `json:"mime_type"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

func viewArtifact(m *artifact.Meta) artifactView {
	return artifactView{ID: m.ID, URI: artifact.URI(m.ID), SessionID: m.SessionID, Name: m.Name,
		MimeType: m.MimeType, Size: m.Size, SHA256: m.SHA256, CreatedAt: m.CreatedAt}
}

func (s *Server) artifacts(w http.ResponseWriter) bool {
	if s.Artifacts == nil {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Error: "artifacts are not configured", Code: codeNotImplemented})
		return false
	}
	return true
}

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.artifacts(w) {
		return
	}
	st, ok := s.session(w, r)
	if !ok {
		return
	}
	sid := st.SessionID
	mimeType := r.Header.Get("Content-Type")
	if mimeType == "application/octet-stream" {
		mimeType = "" // 交给扩展名与内容推断
	}
	m, err := s.Artifacts.Put(r.Context(), sid, r.URL.Query().Get("name"), mimeType, r.Body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewArtifact(m))
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.artifacts(w) {
		return
	}
	if _, ok := s.session(w, r); !ok {
		return
	}
	list, err := s.Artifacts.List(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []artifactView{}
	for _, m := range list {
		out = append(out, viewArtifact(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out})
}

func (s *Server) artifactMeta(w http.ResponseWriter, r *http.Request) {
	m, ok := s.artifact(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, viewArtifact(m))
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	authorized, ok := s.artifact(w, r)
	if !ok {
		return
	}
	m, rc, err := s.Artifacts.Open(r.Context(), authorized.SessionID, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", m.MimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": m.Name}))
	w.Header().Set("X-Yanshi-Session", m.SessionID)
	w.Header().Set("X-Yanshi-Artifact-Name", url.QueryEscape(m.Name))
	_, _ = io.Copy(w, rc)
}
