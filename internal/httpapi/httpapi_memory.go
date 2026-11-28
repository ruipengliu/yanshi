package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/service"
)

func (s *Server) memoryEnabled(w http.ResponseWriter) bool {
	if s.Memory == nil {
		writeJSON(w, http.StatusNotImplemented, errorResponse{Error: "memory is not configured", Code: codeNotImplemented})
		return false
	}
	return true
}

// memoryOwner 返回 Memory 接口作用的 (业务线, EndUser)：用户令牌为本人；服务令牌须给出 end_user。
func memoryOwner(w http.ResponseWriter, r *http.Request, s *Server) (string, string, bool) {
	q := r.URL.Query()
	bl, eu, err := claim(principal(r), q.Get("business_line"), q.Get("end_user"))
	if err != nil {
		s.fail(w, err)
		return "", "", false
	}
	return bl, eu, true
}

// grantOwner 返回 Grant 接口的调用方：授权只能由 EndUser 本人管理，服务令牌一律拒绝。
func grantOwner(w http.ResponseWriter, r *http.Request, s *Server) (string, string, bool) {
	if principal(r).Service() {
		s.fail(w, fmt.Errorf("%w: grants are managed by the end user", errForbidden))
		return "", "", false
	}
	return memoryOwner(w, r, s)
}

type memoryView struct {
	ID            string    `json:"id"`
	Category      string    `json:"category"`
	Content       string    `json:"content"`
	SourceSession string    `json:"source_session"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// 以下只在 ?history=true 列出的历史中出现，或有有效期时出现（m4-memory-grant.md §11）。
	SupersededBy string    `json:"superseded_by,omitempty"`
	SupersededAt time.Time `json:"superseded_at,omitzero"`
	ValidUntil   time.Time `json:"valid_until,omitzero"`
}

func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := memoryOwner(w, r, s)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, err := parseUint(q.Get("limit"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if limit == 0 || limit > 500 {
		limit = 100
	}
	var hits []memory.Hit
	if sources, filtered := q["source_session"]; filtered {
		if p := principal(r); !p.Service() && !p.Unrestricted {
			s.fail(w, fmt.Errorf("%w: source_session requires a service token", errForbidden))
			return
		}
		if len(sources) == 0 || len(sources) > 32 || slices.Contains(sources, "") || q.Get("history") == "true" || q.Get("q") != "" || q.Get("category") != "" || q.Has("limit") {
			s.fail(w, fmt.Errorf("%w: provide 1 to 32 source_session values without history, q, category or limit", service.ErrInvalid))
			return
		}
		// 候选必须完整返回，不能沿用普通列表默认的 100 条截断；有效 Memory 已受每用户上限约束。
		var items []*memory.Memory
		items, err = s.Memory.Store.ListBySource(r.Context(), bl, eu, sources, s.Memory.Clock.Now())
		for _, m := range items {
			hits = append(hits, memory.Hit{Memory: m})
		}
	} else {
		hits, err = s.Memory.List(r.Context(), bl, eu, memory.Category(q.Get("category")), q.Get("q"), q.Get("history") == "true", int(limit))
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []memoryView{}
	for _, h := range hits {
		out = append(out, memoryView{ID: h.ID, Category: string(h.Category), Content: h.Content, SourceSession: h.SourceSession,
			CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt, SupersededBy: h.SupersededBy, SupersededAt: h.SupersededAt, ValidUntil: h.ValidUntil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": out})
}

func (s *Server) forgetMemory(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := memoryOwner(w, r, s)
	if !ok {
		return
	}
	if err := s.Memory.Forget(r.Context(), bl, eu, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// forgetSessionMemories 按来源 Session 撤销 Memory（ADR-0023）：运维发现投毒后，删除由该 Session 写入的全部记忆。
// 只接受服务令牌：这是业务线的运维操作，不是 EndUser 的日常操作（EndUser 用 DELETE /v1/memories/{id}）。
// Session 已删除时按同业务线的删除记录授权，使擦除保留的 Memory 仍可按来源删除。
func (s *Server) forgetSessionMemories(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	id := r.PathValue("id")
	p := principal(r)
	// 与 session 使用相同的归属检查，但暂不写出 404：擦除后仍可能有保留的 Memory。
	st, err := s.Service.Load(r.Context(), id)
	if err == nil && !p.Allows(st.Created.GetBusinessLine(), st.Created.GetEndUser()) {
		err = fmt.Errorf("%w: session %s", service.ErrNotFound, id)
	}
	if errors.Is(err, service.ErrNotFound) && (p.Service() || p.Unrestricted) && s.Service.Deletions != nil {
		bl, ok := businessLineOf(w, r, s)
		if !ok {
			return
		}
		tomb, lookupErr := s.Service.Deletions.Get(r.Context(), id)
		if errors.Is(lookupErr, lifecycle.ErrNotFound) || lookupErr == nil && tomb.BusinessLine != bl {
			err = service.ErrNotFound
		} else {
			err = lookupErr
		}
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !p.Service() && !p.Unrestricted {
		s.fail(w, fmt.Errorf("%w: a service token is required", errForbidden))
		return
	}
	if err := s.Memory.ForgetSession(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) memoryAccess(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := memoryOwner(w, r, s)
	if !ok {
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, fmt.Errorf("%w: since: %v", service.ErrInvalid, err))
			return
		}
		since = t
	}
	list, err := s.Memory.Accesses(r.Context(), eu, bl, since, 500)
	if err != nil {
		s.fail(w, err)
		return
	}
	type view struct {
		MemoryID string    `json:"memory_id"`
		Reader   string    `json:"reader_business_line"`
		At       time.Time `json:"at"`
	}
	out := []view{}
	for _, a := range list {
		out = append(out, view{MemoryID: a.MemoryID, Reader: a.Reader, At: a.At})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accesses": out})
}

type grantView struct {
	ID         string     `json:"id"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	Categories []string   `json:"categories"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func viewGrant(g *memory.Grant) grantView {
	v := grantView{ID: g.ID, From: g.From, To: g.To, Categories: []string{}, CreatedAt: g.CreatedAt}
	for _, c := range g.Categories {
		v.Categories = append(v.Categories, string(c))
	}
	if !g.ExpiresAt.IsZero() {
		v.ExpiresAt = &g.ExpiresAt
	}
	if !g.RevokedAt.IsZero() {
		v.RevokedAt = &g.RevokedAt
	}
	return v
}

func (s *Server) listGrants(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := grantOwner(w, r, s)
	if !ok {
		return
	}
	list, err := s.Memory.ListGrants(r.Context(), eu, bl)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []grantView{}
	for _, g := range list {
		out = append(out, viewGrant(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

func (s *Server) createGrant(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := grantOwner(w, r, s)
	if !ok {
		return
	}
	var req struct {
		From       string     `json:"from"`
		To         string     `json:"to"`
		Categories []string   `json:"categories"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	// 授权界面可以由任一方的 App 发起，但调用方所在的业务线必须是授权的一方。
	if !principal(r).Unrestricted && req.From != bl && req.To != bl {
		s.fail(w, fmt.Errorf("%w: the caller's business line must be a party to the grant", errForbidden))
		return
	}
	cats := make([]memory.Category, len(req.Categories))
	for i, c := range req.Categories {
		cats[i] = memory.Category(c)
	}
	var expires time.Time
	if req.ExpiresAt != nil {
		expires = *req.ExpiresAt
	}
	g, err := s.Memory.CreateGrant(r.Context(), eu, req.From, req.To, cats, expires)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewGrant(g))
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	if !s.memoryEnabled(w) {
		return
	}
	bl, eu, ok := grantOwner(w, r, s)
	if !ok {
		return
	}
	if err := s.Memory.RevokeGrant(r.Context(), eu, bl, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
